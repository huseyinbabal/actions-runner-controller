package actionsmetrics

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	gogithub "github.com/google/go-github/v52/github"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/actions/actions-runner-controller/github"
)

const (
	inProgressJobCheckInterval = 5 * time.Second
)

// InProgressJob stores timing with labels for an in-progress job
type InProgressJob struct {
	StartTime time.Time
	Labels    prometheus.Labels
}

type EventReader struct {
	Log logr.Logger

	// GitHub Client to fetch information about job failures
	GitHubClient *github.Client

	// Event queue
	Events chan interface{}

	// Map of in-progress jobs by job ID
	InProgressJobs map[int64]InProgressJob

	inProgressJobsLock sync.RWMutex
}

// HandleWorkflowJobEvent send event to reader channel for processing
//
// forcing the events through a channel ensures they are processed in sequentially,
// and prevents any race conditions with githubWorkflowJobStatus
func (reader *EventReader) HandleWorkflowJobEvent(event interface{}) {
	reader.Events <- event
}

// ProcessWorkflowJobEvents pop events in a loop for processing
//
// Should be called asynchronously with `go`
func (reader *EventReader) ProcessWorkflowJobEvents(ctx context.Context) {
	// Create a ticker that runs every `inProgressJobCheckInterval`
	ticker := time.NewTicker(inProgressJobCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case event := <-reader.Events:
			reader.ProcessWorkflowJobEvent(ctx, event)
		case <-ticker.C:
			// For all in-progress jobs, increment the metric by 5 seconds using the stored labels
			reader.inProgressJobsLock.Lock()
			for _, jobInfo := range reader.InProgressJobs {
				// By default, the duration is the check interval
				inProgressJobDuration := inProgressJobCheckInterval.Seconds()
				if jobInfo.StartTime.Add(inProgressJobCheckInterval).After(time.Now()) {
					// If the job started less than the check interval ago, use the actual duration
					inProgressJobDuration = time.Since(jobInfo.StartTime).Seconds()
				}
				githubWorkflowJobInProgressDurationSeconds.With(jobInfo.Labels).Add(inProgressJobDuration)
			}
			reader.inProgressJobsLock.Unlock()
		case <-ctx.Done():
			return
		}
	}
}

// ProcessWorkflowJobEvent processes a single event
//
// Events should be processed in the same order that Github emits them
func (reader *EventReader) ProcessWorkflowJobEvent(ctx context.Context, event interface{}) {

	e, ok := event.(*gogithub.WorkflowJobEvent)
	if !ok {
		return
	}

	// collect labels
	var (
		labels        = make(prometheus.Labels)
		keysAndValues = []interface{}{"job_id", fmt.Sprint(*e.WorkflowJob.ID)}
	)

	runsOn := strings.Join(e.WorkflowJob.Labels, `,`)
	labels["runs_on"] = runsOn

	labels["job_name"] = *e.WorkflowJob.Name
	keysAndValues = append(keysAndValues, "job_name", *e.WorkflowJob.Name)

	if e.Repo != nil {
		if n := e.Repo.Name; n != nil {
			labels["repository"] = *n
			keysAndValues = append(keysAndValues, "repository", *n)
		}
		if n := e.Repo.FullName; n != nil {
			labels["repository_full_name"] = *n
			keysAndValues = append(keysAndValues, "repository_full_name", *n)
		}

		if e.Repo.Owner != nil {
			if l := e.Repo.Owner.Login; l != nil {
				labels["owner"] = *l
				keysAndValues = append(keysAndValues, "owner", *l)
			}
		}
	}

	var org string
	if e.Org != nil {
		if n := e.Org.Name; n != nil {
			org = *n
			keysAndValues = append(keysAndValues, "organization", *n)
		}
	}
	labels["organization"] = org

	var wn string
	var hb string
	if e.WorkflowJob != nil {
		if n := e.WorkflowJob.WorkflowName; n != nil {
			wn = *n
			keysAndValues = append(keysAndValues, "workflow_name", *n)
		}
		if n := e.WorkflowJob.HeadBranch; n != nil {
			hb = *n
			keysAndValues = append(keysAndValues, "head_branch", *n)
		}
	}
	labels["workflow_name"] = wn
	labels["head_branch"] = hb

	log := reader.Log.WithValues(keysAndValues...)

	// switch on job status
	switch action := e.GetAction(); action {
	case "queued":
		githubWorkflowJobsQueuedTotal.With(labels).Inc()

	case "in_progress":
		githubWorkflowJobsStartedTotal.With(labels).Inc()

		// Store the start time and labels of this job
		jobID := *e.WorkflowJob.ID
		reader.inProgressJobsLock.Lock()
		// Make a copy of the labels to avoid any potential concurrent modification issues
		labelsCopy := make(prometheus.Labels)
		for k, v := range labels {
			labelsCopy[k] = v
		}
		reader.InProgressJobs[jobID] = InProgressJob{
			StartTime: time.Now(),
			Labels:    labelsCopy,
		}
		reader.inProgressJobsLock.Unlock()

		if reader.GitHubClient == nil {
			return
		}

		parseResult, err := reader.fetchAndParseWorkflowJobLogs(ctx, e)
		if err != nil {
			log.Error(err, "reading workflow job log")
			return
		} else {
			log.Info("reading workflow_job logs")
		}

		githubWorkflowJobQueueDurationSeconds.With(labels).Observe(parseResult.QueueTime.Seconds())

	case "completed":
		githubWorkflowJobsCompletedTotal.With(labels).Inc()

		// Remove the job from tracking since it's no longer in progress
		reader.inProgressJobsLock.Lock()
		delete(reader.InProgressJobs, *e.WorkflowJob.ID)
		reader.inProgressJobsLock.Unlock()

		// job_conclusion -> (neutral, success, skipped, cancelled, timed_out, action_required, failure)
		githubWorkflowJobConclusionsTotal.With(extraLabel("job_conclusion", *e.WorkflowJob.Conclusion, labels)).Inc()

		var (
			exitCode       = "na"
			runTimeSeconds *float64
		)

		// We need to do our best not to fail the whole event processing
		// when the user provided no GitHub API credentials.
		// See https://github.com/actions/actions-runner-controller/issues/2424
		if reader.GitHubClient != nil {
			parseResult, err := reader.fetchAndParseWorkflowJobLogs(ctx, e)
			if err != nil {
				log.Error(err, "reading workflow job log")
				return
			}

			exitCode = parseResult.ExitCode

			s := parseResult.RunTime.Seconds()
			runTimeSeconds = &s

			log.WithValues(keysAndValues...).Info("reading workflow_job logs", "exit_code", exitCode)
		}

		if *e.WorkflowJob.Conclusion == "failure" {
			failedStep := "null"
			for i, step := range e.WorkflowJob.Steps {
				conclusion := step.Conclusion
				if conclusion == nil {
					continue
				}

				// *step.Conclusion ~
				// "success",
				// "failure",
				// "neutral",
				// "cancelled",
				// "skipped",
				// "timed_out",
				// "action_required",
				// null
				if *conclusion == "failure" {
					failedStep = fmt.Sprint(i)
					break
				}
				if *conclusion == "timed_out" {
					failedStep = fmt.Sprint(i)
					exitCode = "timed_out"
					break
				}
			}
			githubWorkflowJobFailuresTotal.With(
				extraLabel("failed_step", failedStep,
					extraLabel("exit_code", exitCode, labels),
				),
			).Inc()
		}

		if runTimeSeconds != nil {
			githubWorkflowJobRunDurationSeconds.With(extraLabel("job_conclusion", *e.WorkflowJob.Conclusion, labels)).Observe(*runTimeSeconds)
		}
	}
}

func extraLabel(key string, value string, labels prometheus.Labels) prometheus.Labels {
	fixedLabels := make(prometheus.Labels)
	for k, v := range labels {
		fixedLabels[k] = v
	}
	fixedLabels[key] = value
	return fixedLabels
}

type ParseResult struct {
	ExitCode  string
	QueueTime time.Duration
	RunTime   time.Duration
}

var logLine = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}.\d{7}Z)\s(.+)$`)
var exitCodeLine = regexp.MustCompile(`##\[error\]Process completed with exit code (\d)\.`)

func (reader *EventReader) fetchAndParseWorkflowJobLogs(ctx context.Context, e *gogithub.WorkflowJobEvent) (*ParseResult, error) {

	owner := *e.Repo.Owner.Login
	repo := *e.Repo.Name
	id := *e.WorkflowJob.ID
	url, _, err := reader.GitHubClient.Actions.GetWorkflowJobLogs(ctx, owner, repo, id, true)
	if err != nil {
		return nil, err
	}
	jobLogs, err := http.DefaultClient.Get(url.String())
	if err != nil {
		return nil, err
	}

	exitCode := "null"

	var (
		queuedTime    time.Time
		startedTime   time.Time
		completedTime time.Time
	)

	func() {
		// Read jobLogs.Body line by line

		defer jobLogs.Body.Close()
		lines := bufio.NewScanner(jobLogs.Body)

		for lines.Scan() {
			matches := logLine.FindStringSubmatch(lines.Text())
			if matches == nil {
				continue
			}
			timestamp := matches[1]
			line := matches[2]

			if strings.HasPrefix(line, "##[error]") {
				// Get exit code
				exitCodeMatch := exitCodeLine.FindStringSubmatch(line)
				if exitCodeMatch != nil {
					exitCode = exitCodeMatch[1]
				}
				continue
			}

			if strings.HasPrefix(line, "Waiting for a runner to pick up this job...") {
				queuedTime, _ = time.Parse(time.RFC3339, timestamp)
				continue
			}

			if strings.HasPrefix(line, "Job is about to start running on the runner:") {
				startedTime, _ = time.Parse(time.RFC3339, timestamp)
				continue
			}

			// Last line in the log will count as the completed time
			completedTime, _ = time.Parse(time.RFC3339, timestamp)
		}
	}()

	return &ParseResult{
		ExitCode:  exitCode,
		QueueTime: startedTime.Sub(queuedTime),
		RunTime:   completedTime.Sub(startedTime),
	}, nil
}

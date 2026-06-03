package scheduler

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/qmuntal/gobuilder/internal/githubactions"
)

type JobQueue interface {
	JobsInQueue(ctx context.Context) (int, error)
}

type WorkflowDispatcher interface {
	DispatchWorkflow(ctx context.Context, dispatchRequest githubactions.DispatchRequest) error
}

type Config struct {
	Queue      JobQueue
	Dispatcher WorkflowDispatcher
	MaxJobs    int
	Workflow   string
	DryRun     bool
	Output     io.Writer
}

type Result struct {
	QueueDepth int
	Dispatched int
}

func Run(ctx context.Context, config Config) (Result, error) {
	if config.Queue == nil {
		return Result{}, fmt.Errorf("queue is required")
	}
	if config.MaxJobs < 0 {
		return Result{}, fmt.Errorf("max jobs must be zero or greater")
	}

	queueDepth, err := config.Queue.JobsInQueue(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("query queue: %w", err)
	}
	if queueDepth < 0 {
		return Result{}, fmt.Errorf("queue returned a negative job count")
	}

	result := Result{QueueDepth: queueDepth, Dispatched: 0}
	dispatchCount := min(queueDepth, config.MaxJobs)

	writeStatus(config.Output, "queue_depth=%d max_jobs=%d dispatch_count=%d\n", queueDepth, config.MaxJobs, dispatchCount)

	if dispatchCount == 0 {
		return result, nil
	}
	if config.DryRun {
		writeStatus(config.Output, "dry_run=true\n")
		return Result{QueueDepth: queueDepth, Dispatched: dispatchCount}, nil
	}
	if config.Dispatcher == nil {
		return result, fmt.Errorf("workflow dispatcher is required")
	}

	type dispatchResult struct {
		jobIndex int
		err      error
	}

	dispatchResults := make(chan dispatchResult, dispatchCount)
	var waitGroup sync.WaitGroup
	for jobIndex := 1; jobIndex <= dispatchCount; jobIndex++ {
		jobIndex := jobIndex
		dispatchRequest := githubactions.DispatchRequest{
			Workflow: config.Workflow,
		}

		waitGroup.Go(func() {
			dispatchResults <- dispatchResult{
				jobIndex: jobIndex,
				err:      config.Dispatcher.DispatchWorkflow(ctx, dispatchRequest),
			}
		})
	}

	waitGroup.Wait()
	close(dispatchResults)

	var dispatchError error
	for dispatchResult := range dispatchResults {
		if dispatchResult.err != nil {
			if dispatchError == nil {
				dispatchError = fmt.Errorf("dispatch builder workflow for job %d: %w", dispatchResult.jobIndex, dispatchResult.err)
			}
			continue
		}

		result.Dispatched++
		writeStatus(config.Output, "dispatched job_index=%d\n", dispatchResult.jobIndex)
	}

	if dispatchError != nil {
		return result, dispatchError
	}

	return result, nil
}

func writeStatus(output io.Writer, format string, args ...any) {
	if output == nil {
		return
	}
	fmt.Fprintf(output, format, args...)
}

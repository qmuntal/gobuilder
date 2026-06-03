package scheduler

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/qmuntal/gobuilder/internal/githubactions"
)

type JobQueue interface {
	JobsInQueue(ctx context.Context) (int, error)
}

type WorkflowDispatcher interface {
	DispatchWorkflow(ctx context.Context, dispatchRequest githubactions.DispatchRequest) error
}

type Config struct {
	Queue          JobQueue
	Dispatcher     WorkflowDispatcher
	MaxJobs        int
	Workflow       string
	SchedulerRunID string
	DryRun         bool
	Output         io.Writer
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

	for jobIndex := 1; jobIndex <= dispatchCount; jobIndex++ {
		dispatchRequest := githubactions.DispatchRequest{
			Workflow: config.Workflow,
			Inputs: map[string]string{
				"job_index":   strconv.Itoa(jobIndex),
				"queue_depth": strconv.Itoa(queueDepth),
			},
		}
		if config.SchedulerRunID != "" {
			dispatchRequest.Inputs["scheduler_run_id"] = config.SchedulerRunID
		}

		if err := config.Dispatcher.DispatchWorkflow(ctx, dispatchRequest); err != nil {
			return result, fmt.Errorf("dispatch builder workflow for job %d: %w", jobIndex, err)
		}

		result.Dispatched++
		writeStatus(config.Output, "dispatched job_index=%d\n", jobIndex)
	}

	return result, nil
}

func writeStatus(output io.Writer, format string, args ...any) {
	if output == nil {
		return
	}
	fmt.Fprintf(output, format, args...)
}

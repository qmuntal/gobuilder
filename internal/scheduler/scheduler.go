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

type WorkflowRunCounter interface {
	ActiveWorkflowRuns(ctx context.Context, workflow string) (githubactions.ActiveWorkflowRuns, error)
}

const (
	botIndexInput = "bot_index"
	maxBotIndex   = 99
)

type Config struct {
	Queue      JobQueue
	Dispatcher WorkflowDispatcher
	RunCounter WorkflowRunCounter
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
	if config.MaxJobs > maxBotIndex {
		return Result{}, fmt.Errorf("max jobs must be %d or less", maxBotIndex)
	}

	queueDepth, err := config.Queue.JobsInQueue(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("query queue: %w", err)
	}
	if queueDepth < 0 {
		return Result{}, fmt.Errorf("queue returned a negative job count")
	}

	activeRuns := githubactions.ActiveWorkflowRuns{BotIndexes: map[int]int{}}
	if config.RunCounter != nil {
		activeRuns, err = config.RunCounter.ActiveWorkflowRuns(ctx, config.Workflow)
		if err != nil {
			return Result{}, fmt.Errorf("count active builder workflow runs: %w", err)
		}
		if activeRuns.Count < 0 || activeRuns.UnknownCount < 0 {
			return Result{}, fmt.Errorf("workflow run counter returned a negative active run count")
		}
	}

	availableSlots := config.MaxJobs - activeRuns.Count
	if availableSlots < 0 {
		availableSlots = 0
	}

	dispatchBotIndexes := allocateBotIndexes(config.MaxJobs, availableSlots, queueDepth, activeRuns)
	result := Result{QueueDepth: queueDepth, Dispatched: 0}
	dispatchCount := len(dispatchBotIndexes)

	writeStatus(config.Output, "queue_depth=%d active_builder_runs=%d unknown_active_builder_runs=%d max_jobs=%d dispatch_count=%d\n", queueDepth, activeRuns.Count, activeRuns.UnknownCount, config.MaxJobs, dispatchCount)

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
		botIndex int
		err      error
	}

	dispatchResults := make(chan dispatchResult, dispatchCount)
	var waitGroup sync.WaitGroup
	for jobIndex, botIndex := range dispatchBotIndexes {
		jobIndex := jobIndex + 1
		botIndex := botIndex
		dispatchRequest := githubactions.DispatchRequest{
			Workflow: config.Workflow,
			Inputs: map[string]string{
				botIndexInput: fmt.Sprintf("%02d", botIndex),
			},
		}

		waitGroup.Go(func() {
			dispatchResults <- dispatchResult{
				jobIndex: jobIndex,
				botIndex: botIndex,
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
		writeStatus(config.Output, "dispatched job_index=%d bot_index=%02d\n", dispatchResult.jobIndex, dispatchResult.botIndex)
	}

	if dispatchError != nil {
		return result, dispatchError
	}

	return result, nil
}

func allocateBotIndexes(maxJobs, availableSlots, queueDepth int, activeRuns githubactions.ActiveWorkflowRuns) []int {
	if maxJobs <= 0 || availableSlots <= 0 || queueDepth <= 0 || activeRuns.UnknownCount > 0 {
		return nil
	}

	dispatchLimit := min(queueDepth, availableSlots)
	botIndexes := make([]int, 0, dispatchLimit)
	for botIndex := 1; botIndex <= maxJobs && len(botIndexes) < dispatchLimit; botIndex++ {
		if activeRuns.BotIndexes[botIndex] > 0 {
			continue
		}
		botIndexes = append(botIndexes, botIndex)
	}
	return botIndexes
}

func writeStatus(output io.Writer, format string, args ...any) {
	if output == nil {
		return
	}
	fmt.Fprintf(output, format, args...)
}

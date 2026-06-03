package scheduler

import (
	"bytes"
	"context"
	"testing"

	"github.com/qmuntal/gobuilder/internal/githubactions"
)

type fakeJobQueue struct {
	jobs int
}

func (queue fakeJobQueue) JobsInQueue(_ context.Context) (int, error) {
	return queue.jobs, nil
}

type recordingDispatcher struct {
	requests []githubactions.DispatchRequest
}

func (dispatcher *recordingDispatcher) DispatchWorkflow(_ context.Context, dispatchRequest githubactions.DispatchRequest) error {
	dispatcher.requests = append(dispatcher.requests, dispatchRequest)
	return nil
}

func TestRunCapsDispatchesAtMaximum(testingContext *testing.T) {
	output := new(bytes.Buffer)
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:          fakeJobQueue{jobs: 5},
		Dispatcher:     dispatcher,
		MaxJobs:        2,
		Workflow:       "builder.yml",
		SchedulerRunID: "12345",
		Output:         output,
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.QueueDepth != 5 {
		testingContext.Fatalf("QueueDepth = %d, want 5", result.QueueDepth)
	}
	if result.Dispatched != 2 {
		testingContext.Fatalf("Dispatched = %d, want 2", result.Dispatched)
	}
	if len(dispatcher.requests) != 2 {
		testingContext.Fatalf("dispatcher requests = %d, want 2", len(dispatcher.requests))
	}
	if dispatcher.requests[1].Inputs["job_index"] != "2" {
		testingContext.Fatalf("second job_index = %q, want 2", dispatcher.requests[1].Inputs["job_index"])
	}
	if dispatcher.requests[0].Inputs["scheduler_run_id"] != "12345" {
		testingContext.Fatalf("scheduler_run_id = %q, want 12345", dispatcher.requests[0].Inputs["scheduler_run_id"])
	}
}

func TestRunDryRunDoesNotRequireDispatcher(testingContext *testing.T) {
	result, err := Run(context.Background(), Config{
		Queue:   fakeJobQueue{jobs: 2},
		MaxJobs: 5,
		DryRun:  true,
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.Dispatched != 2 {
		testingContext.Fatalf("Dispatched = %d, want 2", result.Dispatched)
	}
}

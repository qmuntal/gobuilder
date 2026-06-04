package scheduler

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/qmuntal/gobuilder/internal/githubactions"
)

type fakeJobQueue struct {
	jobs int
}

func (queue fakeJobQueue) JobsInQueue(_ context.Context) (int, error) {
	return queue.jobs, nil
}

type recordingDispatcher struct {
	mutex    sync.Mutex
	requests []githubactions.DispatchRequest
}

func (dispatcher *recordingDispatcher) DispatchWorkflow(_ context.Context, dispatchRequest githubactions.DispatchRequest) error {
	dispatcher.mutex.Lock()
	defer dispatcher.mutex.Unlock()

	dispatcher.requests = append(dispatcher.requests, dispatchRequest)
	return nil
}

func (dispatcher *recordingDispatcher) recordedRequests() []githubactions.DispatchRequest {
	dispatcher.mutex.Lock()
	defer dispatcher.mutex.Unlock()

	requests := make([]githubactions.DispatchRequest, len(dispatcher.requests))
	copy(requests, dispatcher.requests)
	return requests
}

type blockingDispatcher struct {
	started chan struct{}
	release <-chan struct{}
}

func (dispatcher blockingDispatcher) DispatchWorkflow(ctx context.Context, dispatchRequest githubactions.DispatchRequest) error {
	dispatcher.started <- struct{}{}

	select {
	case <-dispatcher.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRunCapsDispatchesAtMaximum(testingContext *testing.T) {
	output := new(bytes.Buffer)
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 5},
		Dispatcher: dispatcher,
		MaxJobs:    2,
		Workflow:   "builder-windows-arm64.yml",
		Output:     output,
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
	requests := dispatcher.recordedRequests()
	if len(requests) != 2 {
		testingContext.Fatalf("dispatcher requests = %d, want 2", len(requests))
	}
	for requestIndex, request := range requests {
		if request.Workflow != "builder-windows-arm64.yml" {
			testingContext.Fatalf("request %d workflow = %q, want builder-windows-arm64.yml", requestIndex, request.Workflow)
		}
		if len(request.Inputs) != 0 {
			testingContext.Fatalf("request %d inputs = %v, want none", requestIndex, request.Inputs)
		}
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

func TestRunDispatchesInParallel(testingContext *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	errChannel := make(chan error, 1)

	go func() {
		_, err := Run(context.Background(), Config{
			Queue:      fakeJobQueue{jobs: 2},
			Dispatcher: blockingDispatcher{started: started, release: release},
			MaxJobs:    2,
			Workflow:   "builder-windows-arm64.yml",
		})
		errChannel <- err
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			testingContext.Fatal("timed out waiting for parallel dispatches to start")
		}
	}

	close(release)
	select {
	case err := <-errChannel:
		if err != nil {
			testingContext.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		testingContext.Fatal("timed out waiting for Run() to finish")
	}
}

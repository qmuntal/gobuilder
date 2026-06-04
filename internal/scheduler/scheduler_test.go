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

type fakeRunCounter struct {
	activeRuns githubactions.ActiveWorkflowRuns
}

func (counter fakeRunCounter) ActiveWorkflowRuns(_ context.Context, _ string) (githubactions.ActiveWorkflowRuns, error) {
	return counter.activeRuns, nil
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
	wantBotIndexes := map[string]bool{"01": true, "02": true}
	for requestIndex, request := range requests {
		if request.Workflow != "builder-windows-arm64.yml" {
			testingContext.Fatalf("request %d workflow = %q, want builder-windows-arm64.yml", requestIndex, request.Workflow)
		}
		botIndex := request.Inputs[botIndexInput]
		if !wantBotIndexes[botIndex] {
			testingContext.Fatalf("request %d bot_index = %q, want one of %v", requestIndex, botIndex, wantBotIndexes)
		}
		delete(wantBotIndexes, botIndex)
	}
}

func TestRunDispatchesStableBotIndexes(testingContext *testing.T) {
	dispatcher := &recordingDispatcher{}

	_, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 3},
		Dispatcher: dispatcher,
		MaxJobs:    3,
		Workflow:   "builder-windows-arm64.yml",
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}

	requests := dispatcher.recordedRequests()
	if len(requests) != 3 {
		testingContext.Fatalf("dispatcher requests = %d, want 3", len(requests))
	}
	wantBotIndexes := map[string]bool{"01": true, "02": true, "03": true}
	for requestIndex, request := range requests {
		botIndex := request.Inputs[botIndexInput]
		if !wantBotIndexes[botIndex] {
			testingContext.Fatalf("request %d bot_index = %q, want one of %v", requestIndex, botIndex, wantBotIndexes)
		}
		delete(wantBotIndexes, botIndex)
	}
}

func TestRunOffsetsBotIndexesByActiveRuns(testingContext *testing.T) {
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 5},
		Dispatcher: dispatcher,
		RunCounter: fakeRunCounter{activeRuns: githubactions.ActiveWorkflowRuns{
			Count:      2,
			BotIndexes: map[int]int{1: 1, 2: 1},
		}},
		MaxJobs:  5,
		Workflow: "builder-windows-arm64.yml",
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.Dispatched != 3 {
		testingContext.Fatalf("Dispatched = %d, want 3", result.Dispatched)
	}

	requests := dispatcher.recordedRequests()
	wantBotIndexes := map[string]bool{"03": true, "04": true, "05": true}
	for requestIndex, request := range requests {
		botIndex := request.Inputs[botIndexInput]
		if !wantBotIndexes[botIndex] {
			testingContext.Fatalf("request %d bot_index = %q, want one of %v", requestIndex, botIndex, wantBotIndexes)
		}
		delete(wantBotIndexes, botIndex)
	}
}

func TestRunUsesFirstFreeBotIndexes(testingContext *testing.T) {
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 5},
		Dispatcher: dispatcher,
		RunCounter: fakeRunCounter{activeRuns: githubactions.ActiveWorkflowRuns{
			Count:      2,
			BotIndexes: map[int]int{2: 1, 4: 1},
		}},
		MaxJobs:  5,
		Workflow: "builder-windows-arm64.yml",
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.Dispatched != 3 {
		testingContext.Fatalf("Dispatched = %d, want 3", result.Dispatched)
	}

	requests := dispatcher.recordedRequests()
	wantBotIndexes := map[string]bool{"01": true, "03": true, "05": true}
	for requestIndex, request := range requests {
		botIndex := request.Inputs[botIndexInput]
		if !wantBotIndexes[botIndex] {
			testingContext.Fatalf("request %d bot_index = %q, want one of %v", requestIndex, botIndex, wantBotIndexes)
		}
		delete(wantBotIndexes, botIndex)
	}
}

func TestRunDoesNotDispatchWhenActiveRunsFillSlots(testingContext *testing.T) {
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 5},
		Dispatcher: dispatcher,
		RunCounter: fakeRunCounter{activeRuns: githubactions.ActiveWorkflowRuns{
			Count:      5,
			BotIndexes: map[int]int{1: 1, 2: 1, 3: 1, 4: 1, 5: 1},
		}},
		MaxJobs:  5,
		Workflow: "builder-windows-arm64.yml",
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.Dispatched != 0 {
		testingContext.Fatalf("Dispatched = %d, want 0", result.Dispatched)
	}
	if requests := dispatcher.recordedRequests(); len(requests) != 0 {
		testingContext.Fatalf("dispatcher requests = %d, want 0", len(requests))
	}
}

func TestRunDoesNotDispatchWithUnknownActiveRunSlot(testingContext *testing.T) {
	dispatcher := &recordingDispatcher{}

	result, err := Run(context.Background(), Config{
		Queue:      fakeJobQueue{jobs: 5},
		Dispatcher: dispatcher,
		RunCounter: fakeRunCounter{activeRuns: githubactions.ActiveWorkflowRuns{
			Count:        1,
			UnknownCount: 1,
			BotIndexes:   map[int]int{},
		}},
		MaxJobs:  5,
		Workflow: "builder-windows-arm64.yml",
	})
	if err != nil {
		testingContext.Fatalf("Run() error = %v", err)
	}
	if result.Dispatched != 0 {
		testingContext.Fatalf("Dispatched = %d, want 0", result.Dispatched)
	}
	if requests := dispatcher.recordedRequests(); len(requests) != 0 {
		testingContext.Fatalf("dispatcher requests = %d, want 0", len(requests))
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

func TestRunRejectsMaxJobsOutsideBotIndexRange(testingContext *testing.T) {
	_, err := Run(context.Background(), Config{
		Queue:   fakeJobQueue{jobs: 1},
		MaxJobs: 100,
	})
	if err == nil {
		testingContext.Fatal("Run() error = nil, want error")
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

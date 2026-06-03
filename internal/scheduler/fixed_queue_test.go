package scheduler

import (
	"context"
	"testing"
)

func TestFixedQueueReturnsConfiguredJobs(testingContext *testing.T) {
	queue, err := NewFixedQueue(2)
	if err != nil {
		testingContext.Fatalf("NewFixedQueue() error = %v", err)
	}

	jobs, err := queue.JobsInQueue(context.Background())
	if err != nil {
		testingContext.Fatalf("JobsInQueue() error = %v", err)
	}
	if jobs != 2 {
		testingContext.Fatalf("JobsInQueue() = %d, want 2", jobs)
	}
}

func TestFixedQueueRejectsNegativeJobs(testingContext *testing.T) {
	_, err := NewFixedQueue(-1)
	if err == nil {
		testingContext.Fatal("NewFixedQueue() error = nil, want error")
	}
}

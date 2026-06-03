package scheduler

import (
	"context"
	"fmt"
)

type FixedQueue struct {
	jobs int
}

func NewFixedQueue(jobs int) (FixedQueue, error) {
	if jobs < 0 {
		return FixedQueue{}, fmt.Errorf("fixed jobs must be zero or greater")
	}

	return FixedQueue{jobs: jobs}, nil
}

func (queue FixedQueue) JobsInQueue(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
		return queue.jobs, nil
	}
}

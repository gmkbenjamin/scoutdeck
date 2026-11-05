package control

import (
	"context"
	"fmt"
)

// Actions allows only one outstanding operation and never blocks the movement loop.
type Actions struct {
	queue chan func(context.Context) error
}

func NewActions() *Actions { return &Actions{queue: make(chan func(context.Context) error)} }

func (a *Actions) Submit(fn func(context.Context) error) error {
	select {
	case a.queue <- fn:
		return nil
	default:
		return fmt.Errorf("another action is running; retry after it finishes")
	}
}

func (a *Actions) Run(ctx context.Context, report func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case fn := <-a.queue:
			if ctx.Err() != nil {
				return
			}
			report(fn(ctx))
		}
	}
}

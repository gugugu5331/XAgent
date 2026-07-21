package orchestrator

import (
	"context"

	"xagent/internal/events"
)

func emitEvent(ctx context.Context, out chan<- events.Event, event events.Event) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case out <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

package provider

import (
	"context"
	"net/http/httptrace"
	"sync"
)

// RequestObserver observes only the stable state of one Provider request
// attempt. It never receives a request or error payload.
type RequestObserver interface {
	MarkSent()
	Finish(sent bool)
}

type requestAttempt struct {
	observer  RequestObserver
	mu        sync.Mutex
	cond      *sync.Cond
	sent      bool
	notifying bool
	finished  bool
}

func newRequestAttempt(observer RequestObserver) *requestAttempt {
	attempt := &requestAttempt{observer: observer}
	attempt.cond = sync.NewCond(&attempt.mu)
	return attempt
}

func (a *requestAttempt) traceContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			a.markSent()
		},
	})
}

func (a *requestAttempt) markSent() {
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.finished || a.sent {
		a.mu.Unlock()
		return
	}
	a.sent = true
	observer := a.observer
	if observer == nil {
		a.mu.Unlock()
		return
	}
	a.notifying = true
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.notifying = false
		a.cond.Broadcast()
		a.mu.Unlock()
	}()
	observer.MarkSent()
}

func (a *requestAttempt) finish() {
	if a == nil {
		return
	}
	a.mu.Lock()
	for a.notifying {
		a.cond.Wait()
	}
	if a.finished {
		a.mu.Unlock()
		return
	}
	a.finished = true
	sent := a.sent
	observer := a.observer
	a.mu.Unlock()
	if observer != nil {
		observer.Finish(sent)
	}
}

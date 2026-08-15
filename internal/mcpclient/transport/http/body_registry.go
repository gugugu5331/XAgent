package http

import (
	"errors"
	"io"
	"sync"
)

var (
	ErrInvalidBody        = errors.New("MCP HTTP response body is invalid")
	ErrBodyRegistryClosed = errors.New("MCP HTTP response body registry is closed")
	ErrBodyCloseFailed    = errors.New("MCP HTTP response body cleanup failed")
)

// bodyRegistry owns every response body from the moment Client.Do returns it
// until its consumer closes the tracked wrapper. Wrapper pointers are the map
// keys so even a non-comparable io.ReadCloser implementation is safe to track.
type bodyRegistry struct {
	mu     sync.Mutex
	closed bool
	bodies map[*trackedBody]struct{}

	closeOnce sync.Once
	closeErr  error
}

func newBodyRegistry() *bodyRegistry {
	return &bodyRegistry{bodies: make(map[*trackedBody]struct{})}
}

func (registry *bodyRegistry) register(body io.ReadCloser) (*trackedBody, error) {
	if registry == nil || body == nil {
		return nil, ErrInvalidBody
	}
	tracked := &trackedBody{body: body, registry: registry}
	registry.mu.Lock()
	if registry.closed {
		registry.mu.Unlock()
		_ = tracked.Close()
		return nil, ErrBodyRegistryClosed
	}
	registry.bodies[tracked] = struct{}{}
	registry.mu.Unlock()
	return tracked, nil
}

func (registry *bodyRegistry) remove(body *trackedBody) {
	if registry == nil || body == nil {
		return
	}
	registry.mu.Lock()
	delete(registry.bodies, body)
	registry.mu.Unlock()
}

func (registry *bodyRegistry) Close() error {
	if registry == nil {
		return nil
	}
	registry.closeOnce.Do(func() {
		registry.mu.Lock()
		registry.closed = true
		bodies := make([]*trackedBody, 0, len(registry.bodies))
		for body := range registry.bodies {
			bodies = append(bodies, body)
		}
		registry.bodies = nil
		registry.mu.Unlock()

		failed := false
		for _, body := range bodies {
			if err := body.Close(); err != nil {
				failed = true
			}
		}
		if failed {
			registry.closeErr = ErrBodyCloseFailed
		}
	})
	return registry.closeErr
}

func (registry *bodyRegistry) activeCount() int {
	if registry == nil {
		return 0
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.bodies)
}

type trackedBody struct {
	body     io.ReadCloser
	registry *bodyRegistry

	closeOnce sync.Once
	closeErr  error
}

func (body *trackedBody) Read(buffer []byte) (int, error) {
	if body == nil || body.body == nil {
		return 0, ErrInvalidBody
	}
	return body.body.Read(buffer)
}

func (body *trackedBody) Close() error {
	if body == nil {
		return nil
	}
	body.closeOnce.Do(func() {
		if body.registry != nil {
			body.registry.remove(body)
		}
		if body.body != nil {
			body.closeErr = body.body.Close()
		}
	})
	return body.closeErr
}

package http

import (
	"context"
	"errors"
	stdhttp "net/http"
	"strings"
	"sync"
)

var ErrRemoteSessionCloseFailed = errors.New("MCP HTTP remote session cleanup failed")

const maxSessionIDBytes = 1024

// remoteSession retains only the latest bounded visible-ASCII session ID. The
// value is protocol state and is never included in returned errors.
type remoteSession struct {
	mu sync.Mutex
	id string
}

func newRemoteSession() *remoteSession {
	return &remoteSession{}
}

func (session *remoteSession) capture(value string) {
	if session == nil || !validSessionID(value) {
		return
	}
	session.mu.Lock()
	session.id = value
	session.mu.Unlock()
}

func (session *remoteSession) current() string {
	if session == nil {
		return ""
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.id
}

func (session *remoteSession) clear(value string) {
	if session == nil {
		return
	}
	session.mu.Lock()
	if session.id == value {
		session.id = ""
	}
	session.mu.Unlock()
}

func validSessionID(value string) bool {
	if len(value) == 0 || len(value) > maxSessionIDBytes || value != strings.TrimSpace(value) {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

// CloseRemote sends the protocol DELETE while the ordinary Close remains
// strictly local. A missing valid session ID means the server created no
// terminable remote session.
func (transport *Transport) CloseRemote(ctx context.Context) error {
	if transport == nil || transport.state == nil || transport.client == nil || transport.session == nil {
		return ErrInvalidConfig
	}
	if err := transport.state.RequireRunning(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID := transport.session.current()
	if sessionID == "" {
		return nil
	}

	requestContext, cancelRequest := transport.requestContext(ctx)
	defer cancelRequest()
	request, err := stdhttp.NewRequestWithContext(requestContext, stdhttp.MethodDelete, transport.target, nil)
	if err != nil {
		return ErrRemoteSessionCloseFailed
	}
	request.Header = transport.headers.Clone()
	request.Header.Set(headerAccept, acceptMCP)
	request.Header.Set(headerSessionID, sessionID)
	if transport.version != "" {
		request.Header.Set(headerProtocolVersion, transport.version)
	}

	response, requestErr := transport.client.Do(request)
	if response != nil && response.Body != nil {
		tracked, trackErr := transport.bodies.register(response.Body)
		if trackErr != nil {
			return ErrRemoteSessionCloseFailed
		}
		response.Body = tracked
	}
	if requestErr != nil {
		_ = closeHTTPResponse(response)
		if requestContext.Err() != nil {
			return requestContext.Err()
		}
		return ErrRemoteSessionCloseFailed
	}
	if response == nil || response.StatusCode < stdhttp.StatusOK || response.StatusCode >= stdhttp.StatusMultipleChoices {
		_ = closeHTTPResponse(response)
		return ErrRemoteSessionCloseFailed
	}
	if err := closeHTTPResponse(response); err != nil {
		return ErrRemoteSessionCloseFailed
	}
	transport.session.clear(sessionID)
	return nil
}

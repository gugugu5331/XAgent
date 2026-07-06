package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const maxStderrSummary = 4096

type StdioConfig struct {
	Command          string
	Args             []string
	Env              map[string]string
	MaxResponseBytes int64
}

type StdioTransport struct {
	config StdioConfig
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	recv   chan RPCResponse

	mu             sync.Mutex
	protocolErrors []string
	stderr         string
	closed         bool
	waitDone       chan error
	stderrDone     chan struct{}
}

func NewStdioTransport(config StdioConfig) *StdioTransport {
	return &StdioTransport{config: config, recv: make(chan RPCResponse, 16)}
}

func (t *StdioTransport) Start(ctx context.Context) error {
	command := strings.TrimSpace(t.config.Command)
	if command == "" {
		return errors.New("stdio command is empty")
	}
	cmd := exec.CommandContext(ctx, command, t.config.Args...)
	cmd.Env = os.Environ()
	for key, value := range t.config.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	configureProcessGroup(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	t.mu.Lock()
	t.cmd = cmd
	t.stdin = stdin
	t.waitDone = make(chan error, 1)
	t.stderrDone = make(chan struct{})
	t.mu.Unlock()

	go t.readStdout(stdout)
	go t.readStderr(stderr)
	go func() {
		t.waitDone <- cmd.Wait()
		close(t.recv)
	}()
	return nil
}

func (t *StdioTransport) Send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	t.mu.Lock()
	stdin := t.stdin
	closed := t.closed
	t.mu.Unlock()
	if closed || stdin == nil {
		return errors.New("stdio transport is not running")
	}

	done := make(chan error, 1)
	go func() {
		_, err := stdin.Write(data)
		done <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func (t *StdioTransport) Recv() <-chan RPCResponse {
	return t.recv
}

func (t *StdioTransport) Close(ctx context.Context) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	stdin := t.stdin
	cmd := t.cmd
	waitDone := t.waitDone
	stderrDone := t.stderrDone
	t.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd == nil || waitDone == nil {
		return nil
	}

	select {
	case err := <-waitDone:
		waitForClosed(stderrDone)
		return ignoreExpectedWaitError(err)
	case <-ctx.Done():
		terminateProcessGroup(cmd)
	}

	killTimer := time.NewTimer(200 * time.Millisecond)
	defer killTimer.Stop()
	select {
	case err := <-waitDone:
		waitForClosed(stderrDone)
		return ignoreExpectedWaitError(err)
	case <-killTimer.C:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}

	select {
	case err := <-waitDone:
		waitForClosed(stderrDone)
		return ignoreExpectedWaitError(err)
	case <-time.After(time.Second):
		return errors.New("stdio process did not exit after kill")
	}
}

func (t *StdioTransport) Diagnostics() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	diagnostics := append([]string(nil), t.protocolErrors...)
	if t.stderr != "" {
		diagnostics = append(diagnostics, t.stderr)
	}
	return diagnostics
}

func (t *StdioTransport) readStdout(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), int(maxResponseBytes(t.config.MaxResponseBytes)))
	for scanner.Scan() {
		line := scanner.Bytes()
		var response RPCResponse
		if err := json.Unmarshal(line, &response); err != nil {
			t.recordProtocolError("malformed stdout JSON-RPC line")
			continue
		}
		if response.JSONRPC != "2.0" {
			t.recordProtocolError("invalid stdout JSON-RPC version")
			continue
		}
		t.recv <- response
	}
	if err := scanner.Err(); err != nil {
		t.recordProtocolError("stdout read error: " + err.Error())
	}
}

func (t *StdioTransport) readStderr(stderr io.Reader) {
	defer close(t.stderrDone)
	data, _ := io.ReadAll(io.LimitReader(stderr, maxStderrSummary+1))
	if len(data) == 0 {
		return
	}
	text := SanitizeMetadata(RedactText(string(data)), maxStderrSummary)
	if len(data) > maxStderrSummary {
		text += "..."
	}
	t.mu.Lock()
	t.stderr = text
	t.mu.Unlock()
}

func (t *StdioTransport) recordProtocolError(message string) {
	t.mu.Lock()
	t.protocolErrors = append(t.protocolErrors, message)
	t.mu.Unlock()
}

func waitForClosed(done <-chan struct{}) {
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func ignoreExpectedWaitError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("stdio process exited: %w", err)
}

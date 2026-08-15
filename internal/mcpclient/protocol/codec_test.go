package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"xagent/internal/budget"
)

func TestDecodeCallToolResultCapturesBeforeCompleteDTO(t *testing.T) {
	var output bytes.Buffer
	frame := json.RawMessage(`{"content":[{"type":"text","text":"first"},{"type":"image","data":"opaque"},{"type":"text","text":"second"}],"isError":true,"structuredContent":{"ok":true}}`)
	result, err := DecodeCallToolResult(frame, &output)
	if err != nil {
		t.Fatalf("decode call tool result: %v", err)
	}
	if got, want := output.String(), "first\nsecond\n[non-text MCP content omitted]\n[structured MCP content omitted]"; got != want {
		t.Fatalf("captured output = %q, want %q", got, want)
	}
	if !result.IsError || len(result.Content) != 3 || result.Content[0].Text != "first" || result.Content[2].Text != "second" {
		t.Fatalf("decoded result = %#v", result)
	}
}

func TestDecodeCapturedCallToolResponseWritesBeforeTailAndScrubsDTO(t *testing.T) {
	const canary = "wire-first-capture-canary-4e91"
	prefix := `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"` + canary + `"}]`
	tail := `,"structuredContent":{"private":"` + canary + `"}}}`
	reader := &gatedCodecReader{prefix: []byte(prefix), release: make(chan struct{})}
	var output bytes.Buffer
	type outcome struct {
		value CapturedCallToolResponse
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		value, err := DecodeCapturedCallToolResponse(json.NewDecoder(reader), &output)
		done <- outcome{value: value, err: err}
	}()

	deadline := time.After(time.Second)
	for output.String() != canary {
		select {
		case <-deadline:
			t.Fatalf("capture was not written before response tail: %q", output.String())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case completed := <-done:
		t.Fatalf("DTO published before tail: %#v", completed)
	default:
	}
	reader.tail = []byte(tail)
	reader.releaseOnce.Do(func() { close(reader.release) })
	completed := <-done
	if completed.err != nil || completed.value.Result.IsError || len(completed.value.Result.Content) != 1 ||
		completed.value.Result.Content[0].Text != "" || len(completed.value.Result.Content[0].Raw) != 0 ||
		completed.value.Result.StructuredContent != nil {
		t.Fatalf("captured response retained user output: %#v / %v", completed.value, completed.err)
	}
}

type gatedCodecReader struct {
	prefix      []byte
	tail        []byte
	release     chan struct{}
	releaseOnce sync.Once
	stage       int
}

func (reader *gatedCodecReader) Read(destination []byte) (int, error) {
	switch reader.stage {
	case 0:
		reader.stage++
		return copy(destination, reader.prefix), nil
	case 1:
		<-reader.release
		reader.stage++
		return copy(destination, reader.tail), nil
	default:
		return 0, io.EOF
	}
}

func TestDecodeCallToolResultRejectsCaptureFailureWithoutPublishingDTO(t *testing.T) {
	result, err := DecodeCallToolResult(json.RawMessage(`{"content":[{"type":"text","text":"secret"}]}`), failingWriter{})
	if !errors.Is(err, ErrResultCapture) {
		t.Fatalf("capture error = %v, want ErrResultCapture", err)
	}
	if result.Content != nil {
		t.Fatalf("result published after capture failure: %#v", result)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRPCIDRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		id   RPCID
		json string
	}{
		{name: "string", id: StringID("1"), json: `"1"`},
		{name: "empty string", id: StringID(""), json: `""`},
		{name: "number", id: NumberID(1), json: `1`},
		{name: "negative number", id: NumberID(-7), json: `-7`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.id)
			if err != nil {
				t.Fatalf("marshal RPC id: %v", err)
			}
			if string(encoded) != test.json {
				t.Fatalf("encoded RPC id = %s, want %s", encoded, test.json)
			}

			var decoded RPCID
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("unmarshal RPC id: %v", err)
			}
			gotKey, err := decoded.Key()
			if err != nil {
				t.Fatalf("decoded RPC id key: %v", err)
			}
			wantKey, err := test.id.Key()
			if err != nil {
				t.Fatalf("expected RPC id key: %v", err)
			}
			if gotKey != wantKey {
				t.Fatalf("round-trip key = %q, want %q", gotKey, wantKey)
			}
		})
	}

	stringKey, err := StringID("1").Key()
	if err != nil {
		t.Fatal(err)
	}
	numberKey, err := NumberID(1).Key()
	if err != nil {
		t.Fatal(err)
	}
	if stringKey == numberKey {
		t.Fatalf("string and number IDs collided at %q", stringKey)
	}

	for _, invalid := range []string{`null`, `true`, `1.5`, `{}`, `[]`} {
		var id RPCID
		if err := json.Unmarshal([]byte(invalid), &id); !errors.Is(err, ErrInvalidRPCID) {
			t.Fatalf("unmarshal invalid ID %s error = %v, want ErrInvalidRPCID", invalid, err)
		}
	}
}

func TestDecodeConsumesBudgetBeforeUnmarshal(t *testing.T) {
	const canary = "mcp-codec-secret-canary-4e91"
	malformed := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":"` + canary)
	counter := newCodecTestCounter(t, int64(len(malformed)+8))
	target := RPCResponse{JSONRPC: "unchanged", ID: StringID("unchanged")}

	err := Decode(counter, malformed, &target)
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("malformed frame error = %v, want ErrMalformedFrame", err)
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatal("malformed frame error exposed payload canary")
	}
	if used := counter.Snapshot().Used(budget.Bytes); used != int64(len(malformed)) {
		t.Fatalf("consumed bytes = %d, want %d before unmarshal", used, len(malformed))
	}
	if target.JSONRPC != "unchanged" {
		t.Fatalf("malformed decode partially published target: %#v", target)
	}

	valid := json.RawMessage(`{"jsonrpc":"2.0","id":"1","result":{}}`)
	err = Decode(counter, valid, &target)
	if err == nil {
		t.Fatal("cumulative frame budget was not enforced")
	}
	var limitErr *budget.LimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("over-limit frame error = %T %v, want budget LimitError", err, err)
	}
	if strings.Contains(err.Error(), string(valid)) || strings.Contains(err.Error(), canary) {
		t.Fatal("budget error exposed frame payload")
	}
	if target.JSONRPC != "unchanged" {
		t.Fatalf("over-limit decode reached unmarshal: %#v", target)
	}

	successCounter := newCodecTestCounter(t, int64(len(valid)))
	if err := Decode(successCounter, valid, &target); err != nil {
		t.Fatalf("decode frame at exact budget: %v", err)
	}
	if target.JSONRPC != "2.0" || !target.ID.IsString() {
		t.Fatalf("decoded response = %#v", target)
	}
}

func newCodecTestCounter(t *testing.T, limit int64) *budget.Counter {
	t.Helper()
	limits, err := budget.NewLimits(budget.Limit{Dimension: budget.Bytes, Value: limit})
	if err != nil {
		t.Fatalf("create codec test limits: %v", err)
	}
	counter, err := budget.NewCounter(limits, limits)
	if err != nil {
		t.Fatalf("create codec test counter: %v", err)
	}
	return counter
}

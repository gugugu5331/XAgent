package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

type RPCID struct {
	String string
	Number *int64
}

func StringID(value string) RPCID {
	return RPCID{String: value}
}

func NumberID(value int64) RPCID {
	return RPCID{Number: &value}
}

func (id RPCID) key() string {
	if id.Number != nil {
		return fmt.Sprintf("number:%d", *id.Number)
	}
	return "string:" + id.String
}

func (id RPCID) MarshalJSON() ([]byte, error) {
	if id.Number != nil {
		return json.Marshal(*id.Number)
	}
	return json.Marshal(id.String)
}

func (id *RPCID) UnmarshalJSON(data []byte) error {
	var number int64
	if err := json.Unmarshal(data, &number); err == nil {
		id.Number = &number
		id.String = ""
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	id.String = text
	id.Number = nil
	return nil
}

type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      RPCID           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type RPCNotification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      RPCID           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("json-rpc error %d: %s", e.Code, e.Message)
}

type Transport interface {
	Send(ctx context.Context, msg any) error
	Recv() <-chan RPCResponse
}

type Connection struct {
	transport      Transport
	nextID         atomic.Int64
	mu             sync.Mutex
	pending        map[string]pendingRequest
	protocolErrors []string
}

type pendingRequest struct {
	method string
	ch     chan RPCResponse
}

func NewConnection(transport Transport) *Connection {
	return &Connection{transport: transport, pending: map[string]pendingRequest{}}
}

func (c *Connection) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case response, ok := <-c.transport.Recv():
				if !ok {
					return
				}
				c.HandleResponse(response)
			}
		}
	}()
}

func (c *Connection) Request(ctx context.Context, method string, params any, result any) error {
	id := NumberID(c.nextID.Add(1))
	paramsJSON, err := marshalOptional(params)
	if err != nil {
		return err
	}
	responseCh := make(chan RPCResponse, 1)
	key := id.key()
	c.mu.Lock()
	if _, exists := c.pending[key]; exists {
		c.mu.Unlock()
		return fmt.Errorf("duplicate pending json-rpc id %s", key)
	}
	c.pending[key] = pendingRequest{method: method, ch: responseCh}
	c.mu.Unlock()

	request := RPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: paramsJSON}
	if err := c.transport.Send(ctx, request); err != nil {
		c.removePending(key)
		return err
	}

	select {
	case <-ctx.Done():
		c.removePending(key)
		return ctx.Err()
	case response := <-responseCh:
		if response.Error != nil {
			return response.Error
		}
		if result == nil || len(response.Result) == 0 {
			return nil
		}
		return json.Unmarshal(response.Result, result)
	}
}

func (c *Connection) Notify(ctx context.Context, method string, params any) error {
	paramsJSON, err := marshalOptional(params)
	if err != nil {
		return err
	}
	return c.transport.Send(ctx, RPCNotification{JSONRPC: "2.0", Method: method, Params: paramsJSON})
}

func (c *Connection) HandleResponse(response RPCResponse) {
	if response.JSONRPC != "2.0" {
		c.recordProtocolError("invalid jsonrpc version")
		return
	}
	key := response.ID.key()
	c.mu.Lock()
	pending, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	if !ok {
		c.recordProtocolError("unknown response id " + key)
		return
	}
	pending.ch <- response
}

func (c *Connection) PendingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func (c *Connection) ProtocolErrors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.protocolErrors...)
}

func (c *Connection) removePending(key string) {
	c.mu.Lock()
	delete(c.pending, key)
	c.mu.Unlock()
}

func (c *Connection) recordProtocolError(message string) {
	c.mu.Lock()
	c.protocolErrors = append(c.protocolErrors, message)
	c.mu.Unlock()
}

func marshalOptional(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	if raw, ok := value.(json.RawMessage); ok {
		return raw, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func ValidateRPCResponse(response RPCResponse) error {
	if response.JSONRPC != "2.0" {
		return errors.New("invalid jsonrpc version")
	}
	return nil
}

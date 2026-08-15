package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

var ErrInvalidRPCID = errors.New("invalid JSON-RPC id")

type rpcIDKind uint8

const (
	rpcIDInvalid rpcIDKind = iota
	rpcIDString
	rpcIDNumber
)

// RPCID preserves whether a JSON-RPC identifier was encoded as a string or
// as an integer. Its fields are private so an identifier cannot represent both
// forms at once.
type RPCID struct {
	kind   rpcIDKind
	text   string
	number int64
}

func StringID(value string) RPCID {
	return RPCID{kind: rpcIDString, text: value}
}

func NumberID(value int64) RPCID {
	return RPCID{kind: rpcIDNumber, number: value}
}

func (id RPCID) IsString() bool {
	return id.kind == rpcIDString
}

func (id RPCID) IsNumber() bool {
	return id.kind == rpcIDNumber
}

func (id RPCID) StringValue() (string, bool) {
	return id.text, id.IsString()
}

func (id RPCID) NumberValue() (int64, bool) {
	return id.number, id.IsNumber()
}

// Key gives pending-request maps a collision-free representation that keeps
// the JSON kind distinct.
func (id RPCID) Key() (string, error) {
	switch id.kind {
	case rpcIDString:
		return "string:" + id.text, nil
	case rpcIDNumber:
		return fmt.Sprintf("number:%d", id.number), nil
	default:
		return "", ErrInvalidRPCID
	}
}

func (id RPCID) MarshalJSON() ([]byte, error) {
	switch id.kind {
	case rpcIDString:
		return json.Marshal(id.text)
	case rpcIDNumber:
		return json.Marshal(id.number)
	default:
		return nil, ErrInvalidRPCID
	}
}

func (id *RPCID) UnmarshalJSON(data []byte) error {
	if id == nil {
		return ErrInvalidRPCID
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil && len(data) > 0 && firstNonSpace(data) == '"' {
		*id = StringID(text)
		return nil
	}

	first := firstNonSpace(data)
	if first == '-' || first >= '0' && first <= '9' {
		var number int64
		if err := json.Unmarshal(data, &number); err == nil {
			*id = NumberID(number)
			return nil
		}
	}

	return ErrInvalidRPCID
}

func firstNonSpace(data []byte) byte {
	for _, value := range data {
		switch value {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return value
		}
	}
	return 0
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

func ValidateRPCResponse(response RPCResponse) error {
	if response.JSONRPC != "2.0" {
		return errors.New("invalid JSON-RPC version")
	}
	if _, err := response.ID.Key(); err != nil {
		return err
	}
	return nil
}

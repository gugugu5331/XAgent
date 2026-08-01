package tool

import (
	"context"
	"encoding/json"
)

type Tool interface {
	Name() string
	Description() string
	Schema() Schema
	Risk() Risk
	Execute(ctx context.Context, input Input) Result
}

type Schema struct {
	Type       string                    `json:"type,omitempty"`
	Properties map[string]SchemaProperty `json:"properties,omitempty"`
	Required   []string                  `json:"required,omitempty"`
	Raw        json.RawMessage           `json:"-"`
}

func (s Schema) MarshalJSON() ([]byte, error) {
	if len(s.Raw) > 0 {
		return s.Raw, nil
	}
	type schema Schema
	return json.Marshal(schema(s))
}

type SchemaProperty struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

type Risk string

const (
	RiskSafe      Risk = "safe"
	RiskDangerous Risk = "dangerous"
)

type Input struct {
	Name         string
	CallID       string
	RawArguments string
	Arguments    map[string]any
}

type Call struct {
	ID            string
	Name          string
	ArgumentsJSON string
}

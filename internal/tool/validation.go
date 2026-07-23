package tool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var errToolNotRegistered = errors.New("tool is not registered")

// ValidatedCall is the single parsed representation of a registered tool
// call. Arguments deliberately preserves json.Number values so the same map
// can flow through hooks, permission fingerprints, and actual execution.
type ValidatedCall struct {
	Call      Call
	Tool      Tool
	Arguments map[string]any
}

// ValidateCall verifies registry membership and parses exactly one JSON
// object. Tool-specific schema checks remain the responsibility of Tool.Execute.
func (r *Registry) ValidateCall(call Call) (ValidatedCall, error) {
	if r == nil {
		return ValidatedCall{}, fmt.Errorf("%w: %q", errToolNotRegistered, call.Name)
	}
	registeredTool, ok := r.Get(call.Name)
	if !ok {
		return ValidatedCall{}, fmt.Errorf("%w: %q", errToolNotRegistered, call.Name)
	}
	arguments := map[string]any{}
	if strings.TrimSpace(call.ArgumentsJSON) != "" {
		decoder := json.NewDecoder(strings.NewReader(call.ArgumentsJSON))
		decoder.UseNumber()
		if err := decoder.Decode(&arguments); err != nil {
			return ValidatedCall{}, fmt.Errorf("tool arguments must be one JSON object: %w", err)
		}
		if arguments == nil {
			return ValidatedCall{}, fmt.Errorf("tool arguments must be one JSON object")
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				err = fmt.Errorf("additional JSON value")
			}
			return ValidatedCall{}, fmt.Errorf("tool arguments must not contain trailing data: %w", err)
		}
	}
	return ValidatedCall{Call: call, Tool: registeredTool, Arguments: arguments}, nil
}

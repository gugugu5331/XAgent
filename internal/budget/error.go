package budget

import (
	"errors"
	"fmt"
)

// ErrIntegerOverflow reports a numeric budget value that cannot be represented
// as a signed 64-bit counter value.
var ErrIntegerOverflow = errors.New("budget integer overflow")

// LimitError contains only safe numeric metadata. It never retains the input
// payload whose size or count crossed the limit.
type LimitError struct {
	Scope     string
	Dimension Dimension
	Limit     int64
	Observed  int64
}

func (e *LimitError) Error() string {
	if e == nil {
		return "budget limit exceeded"
	}
	return fmt.Sprintf(
		"budget limit exceeded: scope=%s dimension=%s limit=%d observed=%d",
		e.Scope,
		e.Dimension,
		e.Limit,
		e.Observed,
	)
}

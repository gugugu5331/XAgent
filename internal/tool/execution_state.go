package tool

// ExecutionState records the lifecycle boundary reached by one tool call.
type ExecutionState string

const (
	Prepared             ExecutionState = "prepared"
	Rejected             ExecutionState = "rejected"
	CancelledBeforeStart ExecutionState = "cancelled_before_start"
	Running              ExecutionState = "running"
	Completed            ExecutionState = "completed"
	CancelledAfterStart  ExecutionState = "cancelled_after_start"
)

// Valid reports whether the state is one of the six fixed lifecycle states.
func (s ExecutionState) Valid() bool {
	switch s {
	case Prepared, Rejected, CancelledBeforeStart, Running, Completed, CancelledAfterStart:
		return true
	default:
		return false
	}
}

// CanProduceResult reports whether the lifecycle state may carry a safe
// result. A call cancelled before crossing its start boundary has no result.
func (s ExecutionState) CanProduceResult() bool {
	switch s {
	case Rejected, Completed, CancelledAfterStart:
		return true
	default:
		return false
	}
}

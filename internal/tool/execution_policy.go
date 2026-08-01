package tool

// ExecutionPolicy describes scheduling properties independently from a
// tool's user-facing risk classification.
type ExecutionPolicy struct {
	ReadOnly       bool
	ConcurrentSafe bool
}

// AllowsConcurrentExecution reports whether the policy satisfies both
// requirements for concurrent scheduling.
func (p ExecutionPolicy) AllowsConcurrentExecution() bool {
	return p.ReadOnly && p.ConcurrentSafe
}

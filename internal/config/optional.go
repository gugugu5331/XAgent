package config

// Optional preserves whether a configuration value was explicitly supplied.
// Value is meaningful even when it is false, zero, or empty if Set is true.
type Optional[T any] struct {
	Set   bool
	Value T
}

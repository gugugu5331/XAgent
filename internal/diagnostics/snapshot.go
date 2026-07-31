package diagnostics

// SnapshotItem is one first-seen safe diagnostic and its aggregate count.
type SnapshotItem struct {
	Diagnostic SafeDiagnostic
	Count      uint64
}

// Snapshot is an immutable point-in-time copy of a BoundedSink.
type Snapshot struct {
	items   []SnapshotItem
	dropped uint64
	bytes   int64
}

func (s Snapshot) Items() []SnapshotItem {
	items := make([]SnapshotItem, len(s.items))
	copy(items, s.items)
	return items
}

func (s Snapshot) Dropped() uint64 {
	return s.dropped
}

func (s Snapshot) Bytes() int64 {
	return s.bytes
}

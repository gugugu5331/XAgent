package subagent

import (
	"fmt"
	"math"
	"time"
)

// ResultProjectionMessageOverheadBytes is the fixed conservative framing
// allowance used when one serialized subagent_result is injected into a
// Provider request. It is shared by config validation and the projector so
// startup cannot accept a window that runtime can never reserve safely.
const ResultProjectionMessageOverheadBytes int64 = 512

func ResultPlanningReserveTokens(maxResultBytes int64) (int64, error) {
	if maxResultBytes <= 0 || maxResultBytes > math.MaxInt64-ResultProjectionMessageOverheadBytes {
		return 0, fmt.Errorf("subagent result planning reserve is invalid")
	}
	bytes := maxResultBytes + ResultProjectionMessageOverheadBytes
	tokens := bytes / 4
	if bytes%4 != 0 {
		tokens++
	}
	return tokens, nil
}

type Limits struct {
	MaxTaskBytes                     int64
	MaxIDBytes                       int64
	MaxRoleNameBytes                 int64
	MaxConcurrent                    int
	MaxQueued                        int
	MaxRetainedTasks                 int
	MaxTaskTombstones                int
	MaxGlobalEvents                  int
	MaxEventsPerTask                 int
	MaxEventBytes                    int64
	MaxSubscriberBuffer              int
	MaxResultBytes                   int64
	MaxPendingResults                int
	MaxResultTotalBytes              int64
	MaxResultsPerClaim               int
	ReadCacheMaxEntries              int
	ReadCacheMaxBytes                int64
	ReadCacheMaxValueBytes           int64
	ReadCacheMaxDependenciesPerEntry int
	AutoBackgroundAfter              time.Duration
	MaxTaskDuration                  time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxTaskBytes:                     32 << 10,
		MaxIDBytes:                       128,
		MaxRoleNameBytes:                 64,
		MaxConcurrent:                    4,
		MaxQueued:                        32,
		MaxRetainedTasks:                 256,
		MaxTaskTombstones:                1_024,
		MaxGlobalEvents:                  8_192,
		MaxEventsPerTask:                 1_024,
		MaxEventBytes:                    64 << 10,
		MaxSubscriberBuffer:              128,
		MaxResultBytes:                   64 << 10,
		MaxPendingResults:                256,
		MaxResultTotalBytes:              8 << 20,
		MaxResultsPerClaim:               32,
		ReadCacheMaxEntries:              128,
		ReadCacheMaxBytes:                8 << 20,
		ReadCacheMaxValueBytes:           1 << 20,
		ReadCacheMaxDependenciesPerEntry: 4_096,
		AutoBackgroundAfter:              10 * time.Second,
	}
}

func (l Limits) Validate() error {
	intFields := []struct {
		name       string
		value, cap int
	}{
		{"max_concurrent", l.MaxConcurrent, 64},
		{"max_queued", l.MaxQueued, 4_096},
		{"max_retained_tasks", l.MaxRetainedTasks, 65_536},
		{"max_task_tombstones", l.MaxTaskTombstones, 65_536},
		{"max_global_events", l.MaxGlobalEvents, 1_000_000},
		{"max_events_per_task", l.MaxEventsPerTask, 100_000},
		{"max_subscriber_buffer", l.MaxSubscriberBuffer, 65_536},
		{"max_pending_results", l.MaxPendingResults, 65_536},
		{"max_results_per_claim", l.MaxResultsPerClaim, 1_024},
		{"read_cache_max_entries", l.ReadCacheMaxEntries, 65_536},
		{"read_cache_max_dependencies_per_entry", l.ReadCacheMaxDependenciesPerEntry, 1_000_000},
	}
	for _, field := range intFields {
		if field.value <= 0 || field.value > field.cap {
			return fmt.Errorf("subagent limit %s is out of range", field.name)
		}
	}
	byteFields := []struct {
		name       string
		value, cap int64
	}{
		{"max_task_bytes", l.MaxTaskBytes, 1 << 20},
		{"max_id_bytes", l.MaxIDBytes, 1 << 10},
		{"max_role_name_bytes", l.MaxRoleNameBytes, 64},
		{"max_event_bytes", l.MaxEventBytes, 1 << 20},
		{"max_result_bytes", l.MaxResultBytes, 1 << 20},
		{"max_result_total_bytes", l.MaxResultTotalBytes, 512 << 20},
		{"read_cache_max_bytes", l.ReadCacheMaxBytes, 512 << 20},
		{"read_cache_max_value_bytes", l.ReadCacheMaxValueBytes, 64 << 20},
	}
	for _, field := range byteFields {
		if field.value <= 0 || field.value > field.cap {
			return fmt.Errorf("subagent limit %s is out of range", field.name)
		}
	}
	if l.MaxConcurrent > int(^uint(0)>>1)-l.MaxQueued || l.MaxRetainedTasks < l.MaxConcurrent+l.MaxQueued {
		return fmt.Errorf("subagent retained task limit is below active capacity")
	}
	if l.MaxEventsPerTask > l.MaxGlobalEvents {
		return fmt.Errorf("subagent per-task event limit exceeds global event limit")
	}
	if l.MaxResultBytes > l.MaxResultTotalBytes {
		return fmt.Errorf("subagent result limit exceeds total result limit")
	}
	if l.ReadCacheMaxValueBytes > l.ReadCacheMaxBytes {
		return fmt.Errorf("subagent read cache value limit exceeds total cache limit")
	}
	if l.AutoBackgroundAfter <= 0 || l.AutoBackgroundAfter > time.Hour {
		return fmt.Errorf("subagent auto background duration is out of range")
	}
	if l.MaxTaskDuration < 0 || l.MaxTaskDuration > 24*time.Hour {
		return fmt.Errorf("subagent task duration is out of range")
	}
	return nil
}

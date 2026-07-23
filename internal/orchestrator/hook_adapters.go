package orchestrator

import (
	"context"
	"math"
	"sync"
	"time"

	"xagent/internal/contextmgr"
	"xagent/internal/hook"
	"xagent/internal/provider"
)

type promptLeaseObserver struct {
	mu       sync.Mutex
	lease    hook.PromptLease
	sent     bool
	finished bool
}

var _ provider.RequestObserver = (*promptLeaseObserver)(nil)

func newPromptLeaseObserver(lease hook.PromptLease) provider.RequestObserver {
	if lease == nil {
		return nil
	}
	return &promptLeaseObserver{lease: lease}
}

func (o *promptLeaseObserver) MarkSent() {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.finished || o.sent {
		o.mu.Unlock()
		return
	}
	o.sent = true
	lease := o.lease
	o.mu.Unlock()
	lease.Commit()
}

func (o *promptLeaseObserver) Finish(sent bool) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.finished {
		o.mu.Unlock()
		return
	}
	o.finished = true
	alreadyCommitted := o.sent
	commit := sent || alreadyCommitted
	o.sent = commit
	lease := o.lease
	o.mu.Unlock()
	if alreadyCommitted {
		return
	}
	if commit {
		lease.Commit()
		return
	}
	lease.Release()
}

type hookCompactionObserver struct {
	runtime hook.Runtime
	binding hook.CompactBinding
	redact  func(string) string
}

var _ contextmgr.CompactionObserver = hookCompactionObserver{}

func (o hookCompactionObserver) Before(ctx context.Context, attempt contextmgr.Attempt) any {
	runtime := o.runtime
	if runtime == nil {
		runtime = hook.Noop()
	}
	reason := hook.CompactAuto
	if attempt.Reason == string(contextmgr.ModeManual) {
		reason = hook.CompactManual
	}
	return runtime.BeforeCompact(ctx, o.binding, hook.CompactInput{
		Reason: reason,
		Before: hook.CompactStats{Messages: attempt.Messages, EstimatedTokens: boundedInt(attempt.EstimatedTokens)},
	})
}

func (o hookCompactionObserver) After(ctx context.Context, token any, result contextmgr.Result, attemptErr error) {
	runtime := o.runtime
	if runtime == nil {
		runtime = hook.Noop()
	}
	compactToken, ok := token.(hook.CompactToken)
	if !ok {
		return
	}
	output := hook.CompactOutput{Status: hook.CompactSuccess}
	if attemptErr != nil {
		output.Status = hook.CompactErrorStatus
		output.Error = attemptErr.Error()
		if o.redact != nil {
			output.Error = o.redact(output.Error)
		}
	}
	output.After = &hook.CompactStats{Messages: result.AfterMessages, EstimatedTokens: boundedInt(result.AfterEstimatedTokens)}
	runtime.AfterCompact(hookLifecycleContext(ctx), compactToken, output)
}

func boundedInt(value int64) int {
	if value > int64(math.MaxInt) {
		return math.MaxInt
	}
	if value < int64(math.MinInt) {
		return math.MinInt
	}
	return int(value)
}

func hookToolStatus(status string) hook.ToolStatus {
	switch status {
	case "success":
		return hook.ToolSuccess
	case "timeout":
		return hook.ToolTimeout
	default:
		return hook.ToolErrorStatus
	}
}

func lifecycleDuration(start time.Time) time.Duration {
	if start.IsZero() {
		return 0
	}
	return time.Since(start)
}

func hookLifecycleContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

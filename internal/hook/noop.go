package hook

import (
	"context"
	"time"
)

type noopRuntime struct{}
type noopPromptLease struct{}

// Noop returns a stateless Runtime suitable for legacy and nil dependency paths.
func Noop() Runtime { return noopRuntime{} }

func (noopRuntime) SystemStart(context.Context)                          {}
func (noopRuntime) Shutdown(context.Context) error                       { return nil }
func (noopRuntime) SessionStart(context.Context, string, SessionState)   {}
func (noopRuntime) SessionEnd(context.Context, string, SessionEndReason) {}
func (noopRuntime) BeginTurn(context.Context, string, ExecutionKind, HookMode) ExecutionRef {
	return ExecutionRef{}
}
func (noopRuntime) EndTurn(context.Context, ExecutionRef, TurnStatus, string) {}
func (noopRuntime) BeginMessage(context.Context, ExecutionRef, MessageRole, string) MessageToken {
	return MessageToken{}
}
func (noopRuntime) EndMessage(context.Context, MessageToken) {}
func (noopRuntime) BeforeTool(context.Context, ExecutionRef, ToolInput) ToolDecision {
	return Continue()
}
func (noopRuntime) AfterTool(context.Context, ExecutionRef, ToolInput, ToolOutput, time.Duration) {}
func (noopRuntime) BeforeCompact(context.Context, CompactBinding, CompactInput) CompactToken {
	return CompactToken{}
}
func (noopRuntime) AfterCompact(context.Context, CompactToken, CompactOutput) {}
func (noopRuntime) AcquirePrompts(context.Context, ExecutionRef) (PromptLease, error) {
	return noopPromptLease{}, nil
}
func (noopPromptLease) Blocks() []PromptBlock { return nil }
func (noopPromptLease) Commit()               {}
func (noopPromptLease) Release()              {}

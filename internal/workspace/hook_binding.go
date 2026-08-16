package workspace

import (
	"context"

	"xagent/internal/hook"
	"xagent/internal/proctree"
	"xagent/internal/safefs"
)

type HookBindRequest struct {
	Root             string
	WorkspaceID      string
	WorkingDirectory *safefs.Root
	Plans            proctree.ProtectionPlanFactory
}

type BoundHook interface {
	hook.Runtime
	Close(context.Context) error
}

type hookBinder interface {
	Bind(context.Context, HookBindRequest) (BoundHook, error)
}

type workspaceHookBinder struct {
	factory *hook.WorkspaceFactory
}

func (b *workspaceHookBinder) Bind(ctx context.Context, request HookBindRequest) (BoundHook, error) {
	if b == nil || b.factory == nil || request.WorkspaceID == "" {
		return nil, ErrRuntimeInvalid
	}
	bound, err := b.factory.Prepare(ctx, hook.WorkspacePrepareRequest{
		ProjectRoot: request.Root, WorkingDirectory: request.WorkingDirectory, Plans: request.Plans,
	})
	if err != nil {
		return nil, err
	}
	return bound, nil
}

var _ hookBinder = (*workspaceHookBinder)(nil)

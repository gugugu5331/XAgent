package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"

	"xagent/internal/config"
	"xagent/internal/instructions"
	"xagent/internal/memory"
	"xagent/internal/redact"
	"xagent/internal/safefs"
	"xagent/internal/sessionctx"
	"xagent/internal/worktree"
)

// ContextFactoryOptions is frozen by NewContextFactory. UserInstructions and
// UserMemory are borrowed shareable dependencies; the factory never closes
// them. Project instructions and memory are rebuilt for every Bind.
type ContextFactoryOptions struct {
	Instructions        config.InstructionsConfig
	InstructionUserRoot string
	// MemoryEnabled is the resolved process-level memory capability. Nil keeps
	// the historical enabled-by-default behavior; false disables both borrowed
	// user memory and task-private project memory for this factory.
	MemoryEnabled    *bool
	Memory           memory.ManagerOptions
	ConfigDigest     string
	UserInstructions sessionctx.InstructionLoader
	UserMemory       sessionctx.MemoryIndexProvider
	Redactor         *redact.RuntimeRedactor
	MaxSectionBytes  int
}

type ContextBindRequest struct {
	Root             string
	WorkspaceID      string
	WorkingDirectory *safefs.Root
}

type BoundContext interface {
	sessionctx.StablePreparer
	Close() error
}

type contextBinder interface {
	Bind(context.Context, ContextBindRequest) (BoundContext, error)
}

type ContextFactory struct {
	options ContextFactoryOptions
}

type boundProjectContext struct {
	root        string
	workspaceID string
	memory      *memory.Manager
	preparer    sessionctx.StablePreparer
	closeOnce   sync.Once
	closeErr    error
}

type fixedProjectDependencies struct {
	root         string
	instructions sessionctx.InstructionLoader
	memory       sessionctx.MemoryIndexProvider
}

func NewContextFactory(options ContextFactoryOptions) (*ContextFactory, error) {
	digest := strings.TrimSpace(options.ConfigDigest)
	if digest == "" || digest != options.ConfigDigest || options.Memory.ProjectDir != "" ||
		(options.InstructionUserRoot != "" && !canonicalContextPath(options.InstructionUserRoot)) ||
		(options.Memory.UserDir != "" && !canonicalContextPath(options.Memory.UserDir)) {
		return nil, ErrRuntimeInvalid
	}
	options.Instructions = cloneContextInstructionsConfig(options.Instructions)
	if options.MemoryEnabled != nil {
		enabled := *options.MemoryEnabled
		options.MemoryEnabled = &enabled
	}
	options.ConfigDigest = digest
	return &ContextFactory{options: options}, nil
}

func (f *ContextFactory) Bind(ctx context.Context, request ContextBindRequest) (BoundContext, error) {
	if f == nil || ctx == nil || ctx.Err() != nil || !worktree.ValidWorkspaceID(request.WorkspaceID) ||
		!canonicalContextPath(request.Root) || request.WorkingDirectory == nil ||
		request.WorkingDirectory.Identity() == (safefs.Identity{}) {
		return nil, ErrRuntimeInvalid
	}
	opened, err := safefs.Bootstrap(request.Root, safefs.Policy{})
	if err != nil {
		return nil, ErrRuntimeInvalid
	}
	identity := opened.Root.Identity()
	closeErr := opened.Root.Close()
	if closeErr != nil || identity != request.WorkingDirectory.Identity() {
		return nil, ErrRuntimeInvalid
	}

	loader, err := instructions.NewWorkspaceLoader(request.Root, f.options.InstructionUserRoot, f.options.Instructions)
	if err != nil {
		return nil, ErrRuntimeInvalid
	}
	var projectMemory sessionctx.MemoryIndexProvider
	var ownedProjectMemory *memory.Manager
	if config.Enabled(f.options.MemoryEnabled, true) {
		memoryOptions := f.options.Memory
		memoryOptions.ProjectDir = filepath.Join(request.Root, ".xagent", "memory")
		ownedProjectMemory, err = memory.NewWorkspaceManager(memory.WorkspaceManagerOptions{
			ManagerOptions: memoryOptions,
			WorktreeRoot:   request.Root,
			ConfigDigest:   f.options.ConfigDigest + "\x00workspace:" + request.WorkspaceID,
		})
		if err != nil {
			return nil, ErrRuntimeInvalid
		}
		projectMemory = ownedProjectMemory
	}
	closeMemory := true
	defer func() {
		if closeMemory && ownedProjectMemory != nil {
			_ = ownedProjectMemory.Close()
		}
	}()

	project := fixedProjectDependencies{root: request.Root, instructions: loader, memory: projectMemory}
	userMemory := f.options.UserMemory
	if !config.Enabled(f.options.MemoryEnabled, true) {
		userMemory = nil
	}
	projectFactory, err := sessionctx.NewProjectFactory(sessionctx.ProjectFactoryOptions{
		UserInstructions: f.options.UserInstructions,
		UserMemory:       userMemory,
		Project:          project,
		Redactor:         f.options.Redactor,
		MaxSectionBytes:  f.options.MaxSectionBytes,
	})
	if err != nil {
		return nil, ErrRuntimeInvalid
	}
	preparer, err := projectFactory.Bind(ctx, request.Root)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil || !sameContextRoot(request.Root, request.WorkingDirectory.Identity()) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, ErrRuntimeInvalid
	}
	closeMemory = false
	return &boundProjectContext{
		root: request.Root, workspaceID: request.WorkspaceID,
		memory: ownedProjectMemory, preparer: preparer,
	}, nil
}

func (d fixedProjectDependencies) BuildProject(ctx context.Context, root string) (sessionctx.ProjectDependencies, error) {
	if ctx == nil || ctx.Err() != nil || root != d.root || d.instructions == nil {
		return sessionctx.ProjectDependencies{}, errors.New("workspace project context dependency binding is invalid")
	}
	return sessionctx.ProjectDependencies{Instructions: d.instructions, Memory: d.memory}, nil
}

func (c *boundProjectContext) PrepareStable(ctx context.Context) sessionctx.PreparedContext {
	if c == nil || c.preparer == nil {
		return sessionctx.PreparedContext{}
	}
	return c.preparer.PrepareStable(ctx)
}

func (c *boundProjectContext) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		if c.memory != nil {
			c.closeErr = c.memory.Close()
		}
	})
	return c.closeErr
}

func cloneContextInstructionsConfig(source config.InstructionsConfig) config.InstructionsConfig {
	clone := source
	if source.Enabled != nil {
		enabled := *source.Enabled
		clone.Enabled = &enabled
	}
	return clone
}

func canonicalContextPath(path string) bool {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	identity, err := memory.NewProjectIdentity(path, "workspace-context-canonicality")
	return err == nil && identity.RootRealPath == path
}

func sameContextRoot(path string, expected safefs.Identity) bool {
	opened, err := safefs.Bootstrap(path, safefs.Policy{})
	if err != nil {
		return false
	}
	actual := opened.Root.Identity()
	return opened.Root.Close() == nil && actual == expected
}

var _ contextBinder = (*ContextFactory)(nil)
var _ BoundContext = (*boundProjectContext)(nil)

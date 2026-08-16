package hook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"xagent/internal/diagnostics"
	"xagent/internal/proctree"
	"xagent/internal/safefs"
)

var (
	ErrWorkspaceFactoryInvalid             = errors.New("workspace hook factory binding is invalid")
	ErrWorkspaceCommandContainmentRequired = errors.New("workspace hook command containment is required")
)

const (
	DiagnosticCommandContainmentFiltered = "hook_command_containment_filtered"
	DiagnosticWorkspaceIdentityChanged   = "hook_workspace_identity_changed"
)

type WorkspaceFactoryOptions struct {
	Snapshot Snapshot
	Engine   EngineOptions
	Runner   proctree.Runner
}

type WorkspacePrepareRequest struct {
	ProjectRoot      string
	WorkingDirectory *safefs.Root
	Plans            proctree.ProtectionPlanFactory
}

type WorkspaceRuntime interface {
	Runtime
	Root() string
	Close(context.Context) error
}

type WorkspaceFactory struct {
	snapshot     Snapshot
	engine       EngineOptions
	runner       proctree.Runner
	openIdentity func(string, safefs.Policy) (safefs.OpenResult, error)
}

type workspaceHookRuntime struct {
	*Engine
	root         string
	identityRoot *safefs.Root
	closeOnce    sync.Once
	closeDone    chan struct{}
	closeErr     error
}

func NewWorkspaceFactory(options WorkspaceFactoryOptions) (*WorkspaceFactory, error) {
	if options.Engine.ProjectRoot != "" || options.Engine.CommandRunner != nil || options.Engine.Diagnostics == nil {
		return nil, ErrWorkspaceFactoryInvalid
	}
	return &WorkspaceFactory{
		snapshot:     newSnapshot(options.Snapshot.Rules()),
		engine:       options.Engine,
		runner:       options.Runner,
		openIdentity: safefs.Bootstrap,
	}, nil
}

func (f *WorkspaceFactory) Prepare(ctx context.Context, request WorkspacePrepareRequest) (WorkspaceRuntime, error) {
	if f == nil || f.openIdentity == nil || ctx == nil || request.WorkingDirectory == nil || !canonicalWorkspaceRoot(request.ProjectRoot) {
		return nil, ErrWorkspaceFactoryInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Plans != nil && f.runner == nil {
		return nil, ErrWorkspaceFactoryInvalid
	}
	identityRoot, err := f.openIdentity(request.ProjectRoot, safefs.Policy{})
	if err != nil || identityRoot.Root.Identity() == (safefs.Identity{}) ||
		identityRoot.Root.Identity() != request.WorkingDirectory.Identity() {
		if identityRoot.Root != nil {
			_ = identityRoot.Root.Close()
		}
		return nil, ErrWorkspaceFactoryInvalid
	}
	rules, filtered, required := workspaceRules(f.snapshot, f.runner != nil && request.Plans != nil)
	if required {
		_ = identityRoot.Root.Close()
		return nil, ErrWorkspaceCommandContainmentRequired
	}
	engineOptions := f.engine
	engineOptions.ProjectRoot = request.ProjectRoot
	if f.runner != nil && request.Plans != nil {
		engineOptions.CommandRunner = &ShellCommandRunner{
			Limits: engineOptions.Limits, Redactor: engineOptions.Redactor,
			Runner: f.runner, Plans: request.Plans, WorkingDirectory: request.WorkingDirectory,
			ProjectRoot: request.ProjectRoot, WorkspaceBound: true, JoinGrace: engineOptions.ShutdownJoinGrace,
		}
	} else {
		engineOptions.CommandRunner = disabledWorkspaceCommandRunner{}
	}
	engine, err := NewEngine(newSnapshot(rules), engineOptions)
	if err != nil {
		_ = identityRoot.Root.Close()
		return nil, err
	}
	engine.factory.bindWorkspaceIdentity(identityRoot.Root.Identity())
	if !engine.factory.validWorkspaceRoot() || ctx.Err() != nil {
		_ = engine.Shutdown(context.Background())
		_ = identityRoot.Root.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, ErrWorkspaceFactoryInvalid
	}
	for range filtered {
		engineOptions.Diagnostics.Add(diagnostics.SanitizeInput{
			Code: DiagnosticCommandContainmentFiltered, Source: "hook.workspace", Hint: "command action filtered",
			Severity: diagnostics.SeverityWarning, Err: errors.New("workspace command action disabled because trusted containment is unavailable"),
		})
	}
	if !engine.factory.validWorkspaceRoot() || ctx.Err() != nil {
		_ = engine.Shutdown(context.Background())
		_ = identityRoot.Root.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, ErrWorkspaceFactoryInvalid
	}
	return &workspaceHookRuntime{
		Engine: engine, root: request.ProjectRoot, identityRoot: identityRoot.Root, closeDone: make(chan struct{}),
	}, nil
}

func workspaceRules(snapshot Snapshot, commandAvailable bool) (rules []Rule, filtered []Rule, required bool) {
	for _, rule := range snapshot.Rules() {
		if rule.action.typeName != ActionCommand || commandAvailable {
			rules = append(rules, rule)
			continue
		}
		// Schema v1 has one mandatory command classification: a synchronous
		// tool_before decision. Dropping it would silently weaken a safety gate.
		if rule.action.decision {
			return nil, nil, true
		}
		filtered = append(filtered, rule)
	}
	return rules, filtered, false
}

func canonicalWorkspaceRoot(root string) bool {
	if root == "" || !utf8.ValidString(root) || strings.TrimSpace(root) != root || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return false
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return false
	}
	canonical, err := canonicalWorkspaceExistingPath(real)
	return err == nil && canonical == root
}

func canonicalWorkspaceExistingPath(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", ErrWorkspaceFactoryInvalid
	}
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	if volume == "" {
		current = string(filepath.Separator)
	}
	remainder := strings.TrimPrefix(path, current)
	if remainder == "" {
		return current, nil
	}
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if !safeWorkspacePathComponent(component) {
			return "", ErrWorkspaceFactoryInvalid
		}
		candidateInfo, err := os.Lstat(filepath.Join(current, component))
		if err != nil {
			return "", err
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return "", err
		}
		actual := ""
		for _, entry := range entries {
			entryInfo, infoErr := entry.Info()
			if infoErr == nil && os.SameFile(candidateInfo, entryInfo) {
				actual = entry.Name()
				break
			}
		}
		if actual == "" {
			return "", ErrWorkspaceFactoryInvalid
		}
		current = filepath.Join(current, actual)
	}
	return filepath.Clean(current), nil
}

func safeWorkspacePathComponent(component string) bool {
	return component != "" && component != "." && component != ".." && filepath.Base(component) == component
}

func (r *workspaceHookRuntime) Root() string {
	if r == nil {
		return ""
	}
	return r.root
}

func (r *workspaceHookRuntime) Shutdown(ctx context.Context) error {
	return r.Close(ctx)
}

func (r *workspaceHookRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.closeOnce.Do(func() {
		go func() {
			var shutdownErr, identityErr error
			if r.Engine != nil {
				shutdownErr = r.Engine.Shutdown(context.Background())
			}
			if r.identityRoot != nil {
				identityErr = r.identityRoot.Close()
			}
			r.closeErr = errors.Join(shutdownErr, identityErr)
			close(r.closeDone)
		}()
	})
	select {
	case <-r.closeDone:
		return r.closeErr
	default:
	}
	select {
	case <-r.closeDone:
		return r.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ WorkspaceRuntime = (*workspaceHookRuntime)(nil)

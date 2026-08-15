package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"xagent/internal/config"
	"xagent/internal/contextmgr"
	"xagent/internal/instructions"
	"xagent/internal/memory"
	"xagent/internal/orchestrator"
	"xagent/internal/resources"
	"xagent/internal/sessionctx"
)

// assemblyContextServices is the context-side portion of the typed Assembly
// graph.  It contains only the already constructed, narrow domain services;
// it deliberately does not retain Assembly, security capabilities, artifact
// stores, or filesystem roots.
//
// The fields are package-private because the service graph is consumed only by
// the orchestration stage in this package.  Keeping the values behind this
// descriptor prevents a second, compatibility construction path from being
// introduced by callers outside cmd/xagent.
type assemblyContextServices struct {
	contextManager     *contextmgr.Manager
	sessionContext     *sessionctx.Manager
	memory             *memory.Manager
	resources          resources.PromptProvider
	requestBudgeter    contextmgr.RequestBudgeter
	skillHistoryPolicy orchestrator.SkillHistoryPolicy
	instructionLoader  *instructions.CachedLoader
}

// assemblyContextRequest supplies the final, resolved Assembly dependencies to
// newAssemblyContextServices.  No partial config or caller-created provider
// is accepted here: configuration and adapters must come from the fixed
// Assembly stages.
type assemblyContextRequest struct {
	paths         RuntimePaths
	configuration *assemblyConfiguration
	execution     *assemblyExecution
	adapters      *assemblyAdapters
}

// newAssemblyContextServices constructs ContextManager, SessionContext,
// Prompt resources, Memory and the two immutable orchestration policies from
// the final Assembly graph.  It is intentionally side-effect free until a
// Memory update is requested; no provider request, artifact read or file is
// opened during construction.
func newAssemblyContextServices(request assemblyContextRequest) (*assemblyContextServices, error) {
	if !validAssemblyContextRequest(request) {
		return nil, errors.New("assembly context dependencies are unavailable")
	}

	resolved := request.configuration.loaded.Config
	redactor := request.configuration.redactor
	providerService := request.adapters.provider
	instructionLoader := request.execution.instructions

	contextManager, err := contextmgr.New(providerService, contextmgr.ManagerOptions{
		Context:           resolved.Context,
		InlineOutputBytes: resolved.Tool.InlineOutputBytes,
		RuntimeRedactor:   redactor,
	})
	if err != nil || contextManager == nil {
		if err == nil {
			err = errors.New("constructor returned nil")
		}
		return nil, fmt.Errorf("context manager: %w", err)
	}

	// Memory paths are resolved against explicit Assembly roots.  In
	// particular, never call os.UserHomeDir or consult process HOME/XDG here:
	// those values are outside the fixed Assembly input contract.
	var memoryManager *memory.Manager
	if config.Enabled(resolved.Memory.Enabled, true) {
		userDir, err := assemblyContextPath(request.paths.UserDataRoot, resolved.Memory.UserDir, "memory user")
		if err != nil {
			return nil, err
		}
		projectDir, err := assemblyContextPath(request.paths.ProjectRoot, resolved.Memory.ProjectDir, "memory project")
		if err != nil {
			return nil, err
		}
		memoryManager = memory.NewManager(memory.ManagerOptions{
			UserDir:           userDir,
			ProjectDir:        projectDir,
			MaxIndexLines:     resolved.Memory.MaxIndexLines,
			MaxIndexBytes:     resolved.Memory.MaxIndexBytes,
			UpdateQueueSize:   resolved.Memory.UpdateQueueSize,
			UpdateConcurrency: resolved.Memory.UpdateConcurrency,
			UpdateTimeoutMS:   resolved.Memory.UpdateTimeoutMS,
			MaxCandidateBytes: resolved.Memory.MaxCandidateBytes,
			Provider:          providerService,
			Redactor:          redactor,
		})
		if memoryManager == nil {
			return nil, errors.New("memory manager: constructor returned nil")
		}
	}

	sessionManager := &sessionctx.Manager{
		Instructions: instructionLoader,
		Context:      contextManager,
		Redactor:     redactor,
	}
	if memoryManager != nil {
		sessionManager.Memory = memoryManager
	}

	skillHistoryPolicy, err := orchestrator.NewSkillHistoryPolicy(
		resolved.Session.MaxSessionBytes,
		resolved.Context.ModelWindowTokens,
		resolved.Context.AutoMarginTokens,
	)
	if err != nil {
		return nil, fmt.Errorf("skill history policy: %w", err)
	}

	requestBudgeter := contextmgr.NewRequestBudgeter()
	if err := requestBudgeter.Validate(); err != nil {
		return nil, fmt.Errorf("request budgeter: %w", err)
	}

	return &assemblyContextServices{
		contextManager:     contextManager,
		sessionContext:     sessionManager,
		memory:             memoryManager,
		resources:          resources.New(),
		requestBudgeter:    requestBudgeter,
		skillHistoryPolicy: skillHistoryPolicy,
		instructionLoader:  instructionLoader,
	}, nil
}

func validAssemblyContextRequest(request assemblyContextRequest) bool {
	return validAssemblyPath(request.paths.ProjectRoot) &&
		validAssemblyPath(request.paths.UserDataRoot) &&
		request.configuration != nil && request.configuration.redactor != nil &&
		request.execution != nil && request.execution.instructions != nil &&
		request.adapters != nil && request.adapters.provider != nil
}

// assemblyContextPath resolves one final config directory from an explicitly
// supplied Assembly root. Absolute config values remain absolute after clean;
// relative values are rooted at the corresponding explicit RuntimePaths entry.
// Both forms are rejected when empty or invalid, and a relative value may not
// escape the explicit root during lexical resolution.
func assemblyContextPath(root, configured, label string) (string, error) {
	if !validAssemblyPath(root) {
		return "", fmt.Errorf("%s root is invalid", label)
	}
	configured = strings.TrimSpace(configured)
	if configured == "" || !utf8AssemblyContext(configured) {
		return "", fmt.Errorf("%s path is invalid", label)
	}
	resolved := configured
	relative := !filepath.IsAbs(resolved)
	if relative {
		resolved = filepath.Join(root, resolved)
	}
	resolved = filepath.Clean(resolved)
	if !validAssemblyPath(resolved) {
		return "", fmt.Errorf("%s path is invalid", label)
	}
	if relative {
		rel, err := filepath.Rel(root, resolved)
		if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%s path escapes its root", label)
		}
	}
	return resolved, nil
}

// Kept local to avoid broadening assembly.go's path helpers.  validAssemblyPath
// also checks UTF-8 and NUL, while this additional check makes the contract
// explicit at the config boundary and keeps error handling deterministic.
func utf8AssemblyContext(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

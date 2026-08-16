package tool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"xagent/internal/permission"
)

// ScopedExecutor is the only ordinary-tool execution boundary owned by a
// child task. Its dependencies are fixed at task construction and its active
// capability view may only narrow when the task detaches.
type ScopedExecutor struct {
	base         *Executor
	verifier     permission.ScopedTicketVerifier
	taskID       string
	capabilities CapabilitySource
	readCache    *ReadCache

	capabilitiesMu sync.Mutex
	lastView       CapabilitySet
}

// NewScopedExecutor binds a safe base executor, one task ticket authority,
// the placement-aware capability source, and that task's private read cache.
// Assembly mistakes fail before a task can start.
func NewScopedExecutor(
	base *Executor,
	verifier permission.TicketVerifier,
	taskID string,
	capabilities CapabilitySource,
	readCache *ReadCache,
) (*ScopedExecutor, error) {
	if base == nil || base.Registry == nil || base.resultFactory == nil {
		return nil, errors.New("scoped executor base is unavailable")
	}
	if !base.Registry.IsSealed() || !base.Registry.safeCandidate {
		return nil, errors.New("scoped executor base registry is not a sealed safe candidate")
	}
	if taskID == "" || strings.TrimSpace(taskID) != taskID {
		return nil, errors.New("scoped executor task ID is invalid")
	}
	scopedVerifier, ok := verifier.(permission.ScopedTicketVerifier)
	if !ok || scopedVerifier == nil || scopedVerifier.ScopeID() != taskID {
		return nil, errors.New("scoped executor verifier does not match the task")
	}
	if capabilities == nil {
		return nil, errors.New("scoped executor capability source is unavailable")
	}
	if !readCacheUsesFactory(readCache, base.resultFactory) {
		return nil, errors.New("scoped executor read cache does not share the result factory")
	}
	initial := capabilities.Current()
	if err := validateScopedCapabilitySet(base.Registry, initial); err != nil {
		return nil, fmt.Errorf("scoped executor capability source is invalid: %w", err)
	}
	return &ScopedExecutor{
		base:         base,
		verifier:     scopedVerifier,
		taskID:       taskID,
		capabilities: capabilities,
		readCache:    readCache,
		lastView:     initial.Clone(),
	}, nil
}

// ExecuteValidatedAuthorized performs the task's final placement-aware
// capability recheck. A rejected call never reaches ticket verification or
// the authorized read-cache boundary.
func (e *ScopedExecutor) ExecuteValidatedAuthorized(
	ctx context.Context,
	validated ValidatedCall,
	ticket permission.ExecutionTicket,
) Result {
	if e == nil || e.base == nil || ctx == nil || ctx.Err() != nil {
		return Result{}
	}
	call := validated.Call
	if !e.base.ownsValidatedCall(validated) {
		return e.filtered(call, FilterUnknown)
	}
	current, err := e.currentCapabilities()
	if err != nil {
		return e.filtered(call, FilterUnknown)
	}
	if !current.Allows(call.Name) {
		return e.filtered(call, current.FilterReason(call.Name))
	}
	return e.base.ExecuteValidatedAuthorizedWithVerifier(ctx, validated, ticket, e.verifier, e.readCache)
}

func (e *ScopedExecutor) currentCapabilities() (CapabilitySet, error) {
	e.capabilitiesMu.Lock()
	defer e.capabilitiesMu.Unlock()
	current := e.capabilities.Current()
	if err := validateScopedCapabilitySet(e.base.Registry, current); err != nil {
		return CapabilitySet{}, err
	}
	if !capabilitySetIsSubset(current, e.lastView) {
		return CapabilitySet{}, errors.New("capability source expanded after construction")
	}
	e.lastView = current.Clone()
	return current, nil
}

func (e *ScopedExecutor) filtered(call Call, reason FilterReason) Result {
	if !reason.valid() {
		reason = FilterUnknown
	}
	result := buildSyntheticResult(e.base.resultFactory, ResultFactoryInput{
		CallID:  call.ID,
		Name:    call.Name,
		State:   Rejected,
		Status:  StatusDenied,
		Summary: "Tool is unavailable in the current task capability view",
		Preview: "The tool is unavailable because the task capability view was narrowed: " + string(reason) + ".",
		Error:   &Error{Code: ErrToolFiltered, Message: "Tool is unavailable in the current task capability view.", Recoverable: true},
	})
	result.Data = map[string]any{"filter_reason": string(reason)}
	return result
}

func readCacheUsesFactory(cache *ReadCache, factory *ResultFactory) bool {
	if cache == nil || factory == nil {
		return false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return !cache.closed && cache.factory == factory
}

func validateScopedCapabilitySet(base *Registry, current CapabilitySet) error {
	if base == nil || !base.IsSealed() || base.lineage == nil {
		return errors.New("base registry is unavailable")
	}
	if current.Registry == nil || !current.Registry.IsSealed() || current.Registry.lineage != base.lineage {
		return errors.New("capability registry does not match the base lineage")
	}
	registryNames := current.Registry.Names()
	if len(current.Names) != len(registryNames) {
		return errors.New("capability names do not match the registry view")
	}
	for index, name := range registryNames {
		if current.Names[index] != name {
			return errors.New("capability names do not match the registry view")
		}
		if _, ok := base.Get(name); !ok {
			return errors.New("capability registry is not a subset of the base")
		}
	}
	for _, name := range base.Names() {
		if current.Allows(name) {
			if _, rejected := current.Rejections[name]; rejected {
				return errors.New("allowed capability also has a rejection reason")
			}
			continue
		}
		reason, rejected := current.Rejections[name]
		if !rejected || !reason.valid() {
			return errors.New("removed capability has no valid rejection reason")
		}
	}
	for name, reason := range current.Rejections {
		if _, ok := base.Get(name); !ok || current.Allows(name) || !reason.valid() {
			return errors.New("capability rejection is inconsistent")
		}
	}
	if current.Fingerprint == "" || current.Fingerprint != capabilityFingerprint(current) {
		return errors.New("capability fingerprint is invalid")
	}
	return nil
}

func capabilitySetIsSubset(candidate, parent CapabilitySet) bool {
	if candidate.Registry == nil || parent.Registry == nil || candidate.Registry.lineage != parent.Registry.lineage {
		return false
	}
	for _, name := range candidate.Registry.Names() {
		if !parent.Allows(name) {
			return false
		}
	}
	return true
}

func (r FilterReason) valid() bool {
	switch r {
	case FilterRoleAllow, FilterRoleDeny, FilterGlobalDeny, FilterBackgroundDeny, FilterPlanMode, FilterRecursive, FilterUnknown:
		return true
	default:
		return false
	}
}

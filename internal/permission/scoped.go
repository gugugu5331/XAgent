package permission

import (
	"errors"
	"strings"

	"xagent/internal/agentrole"
)

var (
	ErrPermissionEscalation  = errors.New("permission_escalation")
	ErrInvalidPermissionMode = errors.New("invalid_permission_mode")
)

// TaskScopeOptions captures the permission state fixed at task creation.
// Permanent permission writes are deliberately unsupported for task scopes.
type TaskScopeOptions struct {
	ScopeID        string
	Mode           Mode
	AllowPermanent bool
}

// TaskScope owns the mutable permission state and execution-ticket authority
// for exactly one task.
type TaskScope struct {
	ScopeID    string
	Authorizer *Authorizer
	Issuer     TicketIssuer
	Verifier   TicketVerifier
}

// RestrictMode applies a role mode to an already effective parent mode. The
// strictness order is strict < default < permissive, so a role may retain or
// lower the rank but may never raise it.
func RestrictMode(parent Mode, role agentrole.PermissionMode) (Mode, error) {
	parent, parentRank, ok := normalizedModeRank(parent)
	if !ok {
		return "", ErrInvalidPermissionMode
	}
	if role == agentrole.PermissionInherit {
		return parent, nil
	}

	requested := Mode(role)
	requested, requestedRank, ok := normalizedModeRank(requested)
	if !ok || role == "" {
		return "", ErrInvalidPermissionMode
	}
	if requestedRank > parentRank {
		return "", ErrPermissionEscalation
	}
	return requested, nil
}

// NewTaskScope snapshots immutable rule layers while allocating a fresh
// session and a scope-bound ticket authority. Parent session grants, writer
// capability, and outstanding tickets are intentionally not copied.
func (a *Authorizer) NewTaskScope(options TaskScopeOptions) (TaskScope, error) {
	if a == nil {
		return TaskScope{}, errors.New("task scope parent authorizer is unavailable")
	}
	if options.ScopeID == "" || strings.TrimSpace(options.ScopeID) != options.ScopeID {
		return TaskScope{}, errors.New("task scope ID is invalid")
	}
	mode, _, ok := normalizedModeRank(options.Mode)
	if !ok {
		return TaskScope{}, ErrInvalidPermissionMode
	}
	if options.AllowPermanent {
		return TaskScope{}, errors.New("task scope permanent permission is unsupported")
	}

	authority, err := newTicketAuthority(options.ScopeID)
	if err != nil {
		return TaskScope{}, err
	}
	loadErrors := cloneLoadErrors(a.LoadErrors)
	loadErrors = appendMissingHealthErrors(loadErrors, a.Health)
	health := NewHealth(authority)
	health.Update(loadErrors)

	child := &Authorizer{
		Session:           NewSession(),
		User:              cloneRuleLayer(a.User),
		Project:           cloneRuleLayer(a.Project),
		Local:             cloneRuleLayer(a.Local),
		LoadErrors:        loadErrors,
		Health:            health,
		Redact:            a.Redact,
		Issuer:            authority,
		fixedMode:         mode,
		permanentDisabled: true,
	}
	return TaskScope{
		ScopeID:    options.ScopeID,
		Authorizer: child,
		Issuer:     authority,
		Verifier:   authority,
	}, nil
}

func cloneRuleLayer(layer RuleLayer) RuleLayer {
	cloned := RuleLayer{Source: layer.Source}
	if layer.Rules != nil {
		cloned.Rules = make([]Rule, len(layer.Rules))
		copy(cloned.Rules, layer.Rules)
	}
	return cloned
}

func cloneLoadErrors(loadErrors []LoadError) []LoadError {
	if loadErrors == nil {
		return nil
	}
	cloned := make([]LoadError, len(loadErrors))
	copy(cloned, loadErrors)
	return cloned
}

func appendMissingHealthErrors(loadErrors []LoadError, health *Health) []LoadError {
	for _, kind := range health.corruptLayerKinds() {
		found := false
		for _, loadError := range loadErrors {
			candidate := loadError.Source.Kind
			if candidate == "" {
				candidate = SourceNone
			}
			if candidate == kind {
				found = true
				break
			}
		}
		if !found {
			loadErrors = append(loadErrors, LoadError{Source: Source{Kind: kind}})
		}
	}
	return loadErrors
}

func normalizedModeRank(mode Mode) (Mode, int, bool) {
	if mode == "" {
		mode = ModeDefault
	}
	switch mode {
	case ModeStrict:
		return mode, 0, true
	case ModeDefault:
		return mode, 1, true
	case ModePermissive:
		return mode, 2, true
	default:
		return "", 0, false
	}
}

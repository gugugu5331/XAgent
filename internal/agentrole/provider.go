package agentrole

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
)

var (
	ErrProviderRegistryInvalid   = errors.New("role provider registry is invalid")
	ErrProviderRegistryDuplicate = errors.New("role provider is already registered")
	ErrProviderRegistryLimit     = errors.New("role provider registry limit reached")
	ErrProviderRegistrySealed    = errors.New("role provider registry is sealed")
)

type ProviderLoadOptions struct {
	Limits Limits
}

type SourceProvider interface {
	ID() string
	LoadRoles(context.Context, ProviderLoadOptions) ([]Candidate, error)
}

type RegisteredProvider struct {
	ProviderID string
	Provider   SourceProvider
}

type ProviderRegistry interface {
	Register(SourceProvider) error
	Seal() error
	Snapshot() []RegisteredProvider
}

type providerRegistry struct {
	mu                 sync.RWMutex
	maxProviders       int
	maxProviderIDBytes int64
	providers          map[string]RegisteredProvider
	sealed             bool
	snapshot           []RegisteredProvider
}

func NewProviderRegistry(maxProviders int, maxProviderIDBytes int64) (ProviderRegistry, error) {
	hard := hardLimits()
	if maxProviders <= 0 || maxProviders > hard.MaxProviders || maxProviderIDBytes <= 0 || maxProviderIDBytes > hard.MaxProviderIDBytes {
		return nil, ErrProviderRegistryInvalid
	}
	return &providerRegistry{
		maxProviders:       maxProviders,
		maxProviderIDBytes: maxProviderIDBytes,
		providers:          make(map[string]RegisteredProvider),
	}, nil
}

func (r *providerRegistry) Register(provider SourceProvider) error {
	if r == nil || isNilProvider(provider) {
		return ErrProviderRegistryInvalid
	}
	providerID, ok := readProviderID(provider)
	if !ok {
		return ErrProviderRegistryInvalid
	}
	normalized, err := normalizeIdentifier(providerID, r.maxProviderIDBytes)
	if err != nil {
		return ErrProviderRegistryInvalid
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrProviderRegistrySealed
	}
	if _, duplicate := r.providers[normalized]; duplicate {
		return ErrProviderRegistryDuplicate
	}
	if len(r.providers) >= r.maxProviders {
		return ErrProviderRegistryLimit
	}
	r.providers[normalized] = RegisteredProvider{ProviderID: normalized, Provider: provider}
	return nil
}

func (r *providerRegistry) Seal() error {
	if r == nil {
		return ErrProviderRegistryInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sealed {
		return ErrProviderRegistrySealed
	}
	providerIDs := make([]string, 0, len(r.providers))
	for providerID := range r.providers {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)
	r.snapshot = make([]RegisteredProvider, 0, len(providerIDs))
	for _, providerID := range providerIDs {
		r.snapshot = append(r.snapshot, r.providers[providerID])
	}
	r.providers = nil
	r.sealed = true
	return nil
}

func (r *providerRegistry) Snapshot() []RegisteredProvider {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.sealed {
		return nil
	}
	return append([]RegisteredProvider(nil), r.snapshot...)
}

func (r *providerRegistry) isSealed() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sealed
}

func isNilProvider(provider SourceProvider) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func readProviderID(provider SourceProvider) (providerID string, ok bool) {
	defer func() {
		if recover() != nil {
			providerID = ""
			ok = false
		}
	}()
	return provider.ID(), true
}

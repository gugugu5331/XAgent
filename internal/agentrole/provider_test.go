package agentrole

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
)

type testRoleProvider struct {
	id      atomic.Value
	idCalls atomic.Int64
	load    func(context.Context, ProviderLoadOptions) ([]Candidate, error)
}

func newTestRoleProvider(id string) *testRoleProvider {
	provider := &testRoleProvider{}
	provider.id.Store(id)
	return provider
}

func (p *testRoleProvider) ID() string {
	p.idCalls.Add(1)
	return p.id.Load().(string)
}

func (p *testRoleProvider) LoadRoles(ctx context.Context, options ProviderLoadOptions) ([]Candidate, error) {
	if p.load == nil {
		return nil, nil
	}
	return p.load(ctx, options)
}

func TestProviderRegistryNormalizesSortsAndFreezesIDs(t *testing.T) {
	registry, err := NewProviderRegistry(4, 64)
	if err != nil {
		t.Fatalf("NewProviderRegistry() error = %v", err)
	}
	zeta := newTestRoleProvider(" ZETA.Provider ")
	alpha := newTestRoleProvider("alpha-provider")
	if err := registry.Register(zeta); err != nil {
		t.Fatalf("Register(zeta) error = %v", err)
	}
	if err := registry.Register(alpha); err != nil {
		t.Fatalf("Register(alpha) error = %v", err)
	}
	if err := registry.Seal(); err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	zeta.id.Store("changed-after-register")
	snapshot := registry.Snapshot()
	wantIDs := []string{"alpha-provider", "zeta.provider"}
	gotIDs := []string{snapshot[0].ProviderID, snapshot[1].ProviderID}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("provider IDs = %v, want %v", gotIDs, wantIDs)
	}
	if zeta.idCalls.Load() != 1 || alpha.idCalls.Load() != 1 {
		t.Fatalf("ID calls after seal/snapshot = %d, %d", zeta.idCalls.Load(), alpha.idCalls.Load())
	}
	snapshot[0].ProviderID = "mutated"
	if registry.Snapshot()[0].ProviderID != "alpha-provider" {
		t.Fatal("Snapshot returned mutable registry storage")
	}
	if err := registry.Seal(); !errors.Is(err, ErrProviderRegistrySealed) {
		t.Fatalf("second Seal() error = %v", err)
	}
	if err := registry.Register(newTestRoleProvider("later")); !errors.Is(err, ErrProviderRegistrySealed) {
		t.Fatalf("Register after seal error = %v", err)
	}
}

func TestProviderRegistryRejectsDuplicateInvalidAndOverLimit(t *testing.T) {
	registry, err := NewProviderRegistry(1, 16)
	if err != nil {
		t.Fatalf("NewProviderRegistry() error = %v", err)
	}
	if err := registry.Register(newTestRoleProvider("Plugin.One")); err != nil {
		t.Fatalf("first Register() error = %v", err)
	}
	if err := registry.Register(newTestRoleProvider(" plugin.one ")); !errors.Is(err, ErrProviderRegistryDuplicate) {
		t.Fatalf("duplicate Register() error = %v", err)
	}
	if err := registry.Register(newTestRoleProvider("second")); !errors.Is(err, ErrProviderRegistryLimit) {
		t.Fatalf("over-limit Register() error = %v", err)
	}

	for _, id := range []string{"", "bad/id", "bad id", "this-provider-id-is-too-long"} {
		fresh, err := NewProviderRegistry(1, 16)
		if err != nil {
			t.Fatal(err)
		}
		if err := fresh.Register(newTestRoleProvider(id)); !errors.Is(err, ErrProviderRegistryInvalid) {
			t.Fatalf("Register(%q) error = %v", id, err)
		}
	}
	if _, err := NewProviderRegistry(0, 16); !errors.Is(err, ErrProviderRegistryInvalid) {
		t.Fatalf("zero provider limit error = %v", err)
	}
	if _, err := NewProviderRegistry(1, 0); !errors.Is(err, ErrProviderRegistryInvalid) {
		t.Fatalf("zero ID limit error = %v", err)
	}
}

func TestProviderRegistryRejectsNilAndPanickingProviders(t *testing.T) {
	registry, err := NewProviderRegistry(2, 64)
	if err != nil {
		t.Fatal(err)
	}
	var nilProvider *testRoleProvider
	if err := registry.Register(nilProvider); !errors.Is(err, ErrProviderRegistryInvalid) {
		t.Fatalf("typed nil provider error = %v", err)
	}
	if err := registry.Register(panickingRoleProvider{}); !errors.Is(err, ErrProviderRegistryInvalid) {
		t.Fatalf("panicking provider error = %v", err)
	}
}

type panickingRoleProvider struct{}

func (panickingRoleProvider) ID() string { panic("unsafe plugin detail") }

func (panickingRoleProvider) LoadRoles(context.Context, ProviderLoadOptions) ([]Candidate, error) {
	return nil, nil
}

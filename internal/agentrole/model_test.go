package agentrole

import (
	"errors"
	"reflect"
	"testing"
)

func TestModelCatalogResolvesFixedAliasesAndInheritedModel(t *testing.T) {
	validated := make([]string, 0, 5)
	catalog, err := NewModelCatalog(ModelCatalogOptions{
		ProviderID:   "anthropic",
		DefaultModel: "claude-default",
		Aliases: ModelAliases{
			Haiku:  "claude-haiku",
			Sonnet: "claude-sonnet",
		},
		MaxModelBytes: 256,
		Validate: func(model string) error {
			validated = append(validated, model)
			if model == "invalid-parent" {
				return errors.New("unsupported")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewModelCatalog() error = %v", err)
	}
	if got, err := catalog.Resolve(ModelHaiku, ""); err != nil || got != "claude-haiku" {
		t.Fatalf("Resolve(haiku) = %q, %v", got, err)
	}
	if got, err := catalog.Resolve(ModelInherit, "captured-parent"); err != nil || got != "captured-parent" {
		t.Fatalf("Resolve(inherit, parent) = %q, %v", got, err)
	}
	if got, err := catalog.Resolve(ModelInherit, ""); err != nil || got != "claude-default" {
		t.Fatalf("Resolve(inherit, default) = %q, %v", got, err)
	}
	if len(validated) < 4 {
		t.Fatalf("validator calls = %v, want constructor mappings and inherited model", validated)
	}
}

func TestModelCatalogReportsUnavailableAsTypedError(t *testing.T) {
	catalog, err := NewModelCatalog(ModelCatalogOptions{
		ProviderID:    "anthropic",
		DefaultModel:  "claude-default",
		MaxModelBytes: 256,
		Validate: func(model string) error {
			if model == "invalid-parent" {
				return errors.New("unsupported")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewModelCatalog() error = %v", err)
	}
	for _, test := range []struct {
		name      string
		alias     ModelAlias
		inherited string
	}{
		{name: "missing mapping", alias: ModelOpus},
		{name: "unknown alias", alias: ModelAlias("fast")},
		{name: "invalid inherited model", alias: ModelInherit, inherited: "invalid-parent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, gotErr := catalog.Resolve(test.alias, test.inherited)
			var resolutionErr *ModelResolutionError
			if !errors.As(gotErr, &resolutionErr) {
				t.Fatalf("Resolve() error = %T %v, want *ModelResolutionError", gotErr, gotErr)
			}
			if resolutionErr.Alias != test.alias || resolutionErr.Reason != ErrModelAliasUnavailable {
				t.Fatalf("resolution error = %#v", resolutionErr)
			}
			if gotErr.Error() != string(ErrModelAliasUnavailable) {
				t.Fatalf("safe error text = %q", gotErr.Error())
			}
		})
	}
}

func TestModelCatalogMetadataIsSortedAndCopied(t *testing.T) {
	catalog, err := NewModelCatalog(ModelCatalogOptions{
		ProviderID:    "anthropic",
		DefaultModel:  "claude-default",
		Aliases:       ModelAliases{Opus: "claude-opus"},
		MaxModelBytes: 256,
		Validate:      func(string) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewModelCatalog() error = %v", err)
	}
	models := catalog.Models()
	wantAliases := []ModelAlias{ModelHaiku, ModelInherit, ModelOpus, ModelSonnet}
	gotAliases := make([]ModelAlias, 0, len(models))
	for _, model := range models {
		gotAliases = append(gotAliases, model.Alias)
		if model.Provider != "anthropic" {
			t.Fatalf("provider = %q", model.Provider)
		}
	}
	if !reflect.DeepEqual(gotAliases, wantAliases) {
		t.Fatalf("aliases = %v, want %v", gotAliases, wantAliases)
	}
	if !models[1].Available || !models[1].Dynamic || models[1].Concrete != "claude-default" {
		t.Fatalf("inherit metadata = %#v", models[1])
	}
	if models[0].Available || models[3].Available || !models[2].Available {
		t.Fatalf("fixed metadata availability = %#v", models)
	}
	models[0].Concrete = "mutated"
	if catalog.Models()[0].Concrete == "mutated" {
		t.Fatal("Models returned mutable catalog storage")
	}
}

func TestNewModelCatalogRejectsUnsafeConfiguration(t *testing.T) {
	valid := ModelCatalogOptions{
		ProviderID:    "anthropic",
		DefaultModel:  "claude-default",
		MaxModelBytes: 256,
		Validate:      func(string) error { return nil },
	}
	for _, test := range []struct {
		name   string
		mutate func(*ModelCatalogOptions)
	}{
		{name: "empty provider", mutate: func(options *ModelCatalogOptions) { options.ProviderID = "" }},
		{name: "empty default", mutate: func(options *ModelCatalogOptions) { options.DefaultModel = "" }},
		{name: "missing validator", mutate: func(options *ModelCatalogOptions) { options.Validate = nil }},
		{name: "invalid max bytes", mutate: func(options *ModelCatalogOptions) { options.MaxModelBytes = 0 }},
		{name: "oversize model", mutate: func(options *ModelCatalogOptions) { options.Aliases.Haiku = string(make([]byte, 257)) }},
		{name: "provider rejects model", mutate: func(options *ModelCatalogOptions) {
			options.Validate = func(string) error { return errors.New("not supported") }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := valid
			test.mutate(&options)
			if _, err := NewModelCatalog(options); err == nil {
				t.Fatal("NewModelCatalog() unexpectedly succeeded")
			}
		})
	}
}

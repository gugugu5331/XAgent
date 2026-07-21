package command

import "testing"

func completionRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, err := New(
		Definition{Name: "plan", Aliases: []string{"p"}, Type: TypeUI, Handler: noopHandler},
		Definition{Name: "permission", Aliases: []string{"perm"}, Type: TypeLocal, Handler: noopHandler},
		Definition{Name: "compact", Aliases: []string{"ctx"}, Type: TypeLocal, Handler: noopHandler},
		Definition{Name: "clear", Aliases: []string{"cls"}, Type: TypeUI, Handler: noopHandler},
		Definition{Name: "private", Hidden: true, Type: TypeLocal, Handler: noopHandler},
	)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestCompleteUniquePrefixAndExactAlias(t *testing.T) {
	registry := completionRegistry(t)
	if got := registry.Complete("/comp"); len(got) != 1 || got[0].Name != "compact" {
		t.Fatalf("unexpected unique completion: %#v", got)
	}
	if got := registry.Complete("/p"); len(got) != 1 || got[0].Name != "plan" {
		t.Fatalf("exact alias did not win: %#v", got)
	}
}

func TestCompleteMultipleResultsAreSortedAndDeduplicated(t *testing.T) {
	registry := completionRegistry(t)
	got := registry.Complete("/c")
	if len(got) != 2 || got[0].Name != "clear" || got[1].Name != "compact" {
		t.Fatalf("unexpected completions: %#v", got)
	}
}

func TestCompleteExcludesHiddenAndArgumentInputs(t *testing.T) {
	registry := completionRegistry(t)
	if got := registry.Complete("/priv"); len(got) != 0 {
		t.Fatalf("hidden completion leaked: %#v", got)
	}
	for _, input := range []string{"", "plan", "/plan ", "/plan arg"} {
		if got := registry.Complete(input); len(got) != 0 {
			t.Fatalf("unexpected completion for %q: %#v", input, got)
		}
	}
}

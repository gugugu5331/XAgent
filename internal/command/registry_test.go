package command

import (
	"strings"
	"testing"
)

func noopHandler(ExecutionContext, Invocation) error { return nil }

func TestRegistryNormalizesSortsAndCopiesDefinitions(t *testing.T) {
	definitions := []Definition{
		{Name: "/Zulu", Aliases: []string{"Z"}, Type: TypeLocal, Handler: noopHandler},
		{Name: "alpha", Aliases: []string{"A"}, Type: TypeUI, Handler: noopHandler},
	}
	registry, err := New(definitions...)
	if err != nil {
		t.Fatal(err)
	}
	definitions[0].Aliases[0] = "changed"
	visible := registry.Visible()
	if len(visible) != 2 || visible[0].Name != "alpha" || visible[1].Name != "zulu" || visible[1].Aliases[0] != "z" {
		t.Fatalf("unexpected definitions: %#v", visible)
	}
	visible[0].Aliases[0] = "mutated"
	if registry.Visible()[0].Aliases[0] != "a" {
		t.Fatal("Visible returned mutable registry aliases")
	}
}

func TestRegistryRejectsConflicts(t *testing.T) {
	tests := []struct {
		name        string
		definitions []Definition
		conflict    string
	}{
		{name: "name name", conflict: "same", definitions: []Definition{{Name: "same", Type: TypeLocal, Handler: noopHandler}, {Name: "SAME", Type: TypeUI, Handler: noopHandler}}},
		{name: "name alias", conflict: "one", definitions: []Definition{{Name: "one", Type: TypeLocal, Handler: noopHandler}, {Name: "two", Aliases: []string{"ONE"}, Type: TypeLocal, Handler: noopHandler}}},
		{name: "alias alias", conflict: "x", definitions: []Definition{{Name: "one", Aliases: []string{"x"}, Type: TypeLocal, Handler: noopHandler}, {Name: "two", Aliases: []string{"X"}, Type: TypeLocal, Handler: noopHandler}}},
		{name: "inside definition", conflict: "dup", definitions: []Definition{{Name: "one", Aliases: []string{"dup", "DUP"}, Type: TypeLocal, Handler: noopHandler}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.definitions...)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), test.conflict) {
				t.Fatalf("expected conflict %q, got %v", test.conflict, err)
			}
		})
	}
}

func TestRegistryRejectsInvalidDefinitions(t *testing.T) {
	tests := []Definition{
		{Name: "", Type: TypeLocal, Handler: noopHandler},
		{Name: "two words", Type: TypeLocal, Handler: noopHandler},
		{Name: "name", Aliases: []string{""}, Type: TypeLocal, Handler: noopHandler},
		{Name: "name", Type: Type("bad"), Handler: noopHandler},
		{Name: "name", Type: TypeLocal},
	}
	for index, definition := range tests {
		if _, err := New(definition); err == nil {
			t.Fatalf("case %d unexpectedly succeeded", index)
		}
	}
}

func TestMustNewPanicsWithConflictDetails(t *testing.T) {
	defer func() {
		value := recover()
		if value == nil {
			t.Fatal("expected panic")
		}
		message := strings.ToLower(value.(error).Error())
		if !strings.Contains(message, "dup") || !strings.Contains(message, "one") || !strings.Contains(message, "two") {
			t.Fatalf("panic lacks conflict details: %q", message)
		}
	}()
	MustNew(
		Definition{Name: "one", Aliases: []string{"dup"}, Type: TypeLocal, Handler: noopHandler},
		Definition{Name: "two", Aliases: []string{"DUP"}, Type: TypeLocal, Handler: noopHandler},
	)
}

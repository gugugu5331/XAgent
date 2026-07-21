package tui

import (
	"strings"
	"testing"
)

func TestCommandMenuOpenCopiesAndRendersItems(t *testing.T) {
	items := []CommandMenuItem{{Name: "clear", Description: "清屏", ArgHint: "[scope]"}, {Name: "compact", Description: "压缩"}}
	var menu CommandMenu
	menu.Open(items)
	items[0].Name = "changed"
	if !menu.Visible || menu.Selected != 0 || menu.Items[0].Name != "clear" {
		t.Fatalf("unexpected menu state: %#v", menu)
	}
	output := menu.View()
	for _, want := range []string{"› /clear", "[scope]", "清屏", "/compact", "压缩"} {
		if !strings.Contains(output, want) {
			t.Fatalf("menu output missing %q: %q", want, output)
		}
	}
}

func TestCommandMenuMovesWithWrapAndSelects(t *testing.T) {
	menu := CommandMenu{}
	menu.Open([]CommandMenuItem{{Name: "one"}, {Name: "two"}})
	menu.Move(-1)
	if item, ok := menu.SelectedItem(); !ok || item.Name != "two" {
		t.Fatalf("unexpected wrapped item: %#v %t", item, ok)
	}
	menu.Move(1)
	if item, ok := menu.SelectedItem(); !ok || item.Name != "one" {
		t.Fatalf("unexpected moved item: %#v %t", item, ok)
	}
	menu.Close()
	if menu.Visible || len(menu.Items) != 0 {
		t.Fatalf("menu did not close: %#v", menu)
	}
}

func TestCommandMenuEmptyDoesNotOpen(t *testing.T) {
	menu := CommandMenu{}
	menu.Open(nil)
	if menu.Visible || menu.View() != "" {
		t.Fatalf("empty menu is visible: %#v", menu)
	}
}

func TestCommandMenuBadge(t *testing.T) {
	var menu CommandMenu
	menu.Open([]CommandMenuItem{
		{Name: "commit", Description: "提交改动", Badge: "Skill/shared"},
		{Name: "review", Description: "审查改动", Badge: "Skill/isolated"},
	})
	output := menu.View()
	for _, want := range []string{"/commit [Skill/shared]", "/review [Skill/isolated]"} {
		if !strings.Contains(output, want) {
			t.Fatalf("menu output missing %q: %q", want, output)
		}
	}
}

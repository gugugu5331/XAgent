package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRejectsWorkspaceRoot(t *testing.T) {
	workspace := t.TempDir()
	cases := []struct {
		name string
		root string
	}{
		{name: "workspace", root: workspace},
		{name: "workspace child", root: filepath.Join(workspace, "private", "artifacts")},
		{name: "workspace parent", root: filepath.Dir(workspace)},
	}
	outside := t.TempDir()
	alias := filepath.Join(outside, "workspace-alias")
	if err := os.Symlink(workspace, alias); err == nil {
		cases = append(cases, struct {
			name string
			root string
		}{name: "symlink alias child", root: filepath.Join(alias, "artifacts")})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store, err := NewFileStore(FileStoreOptions{Root: test.root, WorkspaceRoot: workspace})
			if err == nil || store != nil {
				t.Fatal("workspace artifact root was accepted")
			}
			if strings.Contains(err.Error(), workspace) || strings.Contains(err.Error(), test.root) {
				t.Fatal("root rejection exposed a private path")
			}
		})
	}

	outsideRoot := filepath.Join(t.TempDir(), "artifacts")
	store, err := NewFileStore(FileStoreOptions{Root: outsideRoot, WorkspaceRoot: workspace})
	if err != nil {
		t.Fatal("outside artifact root was rejected")
	}
	if _, err := os.Stat(outsideRoot); !os.IsNotExist(err) {
		t.Fatal("store validation created the artifact root")
	}
	if err := store.Close(); err != nil {
		t.Fatal("store close failed")
	}
	if err := store.Close(); err != nil {
		t.Fatal("repeated store close changed the result")
	}
}

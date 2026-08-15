package repoaudit

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLegacyToolResultPathsHaveNoProductionReference(t *testing.T) {
	root := t429aRepositoryRoot(t)
	commandRoot := filepath.Join(root, "cmd", "xagent")
	forbidden := []string{"defaultStartupFactories", "runWithFactories", "newCompositeCloser"}
	err := filepath.WalkDir(commandRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, marker := range forbidden {
			if strings.Contains(string(body), marker) {
				t.Errorf("legacy production entry %q remains in %s", marker, contextResultRelativePath(root, path))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := os.ReadFile(filepath.Join(commandRoot, "assembly.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(assembly)
	for _, required := range []string{"tool.NewSafeCandidateRegistry()", "app.NewWithOptions(deps, runtimeOptions)"} {
		if !strings.Contains(text, required) {
			t.Errorf("Assembly safe production path is missing %q", required)
		}
	}
}

func TestModelContentHasNoPersistencePath(t *testing.T) {
	root := t429aRepositoryRoot(t)
	contextRoot := filepath.Join(root, "internal", "contextmgr")
	err := filepath.WalkDir(contextRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)
		if strings.Contains(text, "ModelContent()") {
			for _, forbidden := range []string{"filepath.", "os.Write", "os.Open", "artifact.Store", "conversation.Store"} {
				if strings.Contains(text, forbidden) {
					t.Errorf("ModelContent shares a production file with persistence capability %q in %s", forbidden, contextResultRelativePath(root, path))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func t429aRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve T4.29a audit source failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
}

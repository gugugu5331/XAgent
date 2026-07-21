package skill

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed builtins
var embeddedBuiltins embed.FS

func BuiltinSource() SourceFS {
	return SourceFS{Source: SourceBuiltin, FS: embeddedBuiltins, Root: "builtins"}
}

func MaterializeBuiltin(name string) (string, func() error, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !skillNamePattern.MatchString(name) {
		return "", nil, fmt.Errorf("invalid builtin skill name")
	}
	sourceRoot := path.Join("builtins", name)
	if _, err := fs.Stat(embeddedBuiltins, path.Join(sourceRoot, "SKILL.md")); err != nil {
		return "", nil, fmt.Errorf("builtin skill does not exist")
	}
	temporaryRoot, err := os.MkdirTemp("", "xagent-skill-"+name+"-")
	if err != nil {
		return "", nil, fmt.Errorf("create builtin skill temporary directory: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(temporaryRoot) }
	if err := os.Chmod(temporaryRoot, 0o700); err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("secure builtin skill temporary directory: %w", err)
	}
	err = fs.WalkDir(embeddedBuiltins, sourceRoot, func(sourcePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if sourcePath == sourceRoot {
			return nil
		}
		relativePath := strings.TrimPrefix(sourcePath, sourceRoot+"/")
		if relativePath == sourcePath || relativePath == ".." || strings.HasPrefix(relativePath, "../") {
			return fmt.Errorf("invalid embedded builtin path")
		}
		destination := filepath.Join(temporaryRoot, filepath.FromSlash(relativePath))
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		if entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported embedded builtin resource")
		}
		data, err := fs.ReadFile(embeddedBuiltins, sourcePath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		return os.WriteFile(destination, data, 0o600)
	})
	if err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("materialize builtin skill: %w", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(temporaryRoot)
	if err != nil {
		_ = cleanup()
		return "", nil, fmt.Errorf("resolve builtin skill temporary directory: %w", err)
	}
	return canonicalRoot, cleanup, nil
}

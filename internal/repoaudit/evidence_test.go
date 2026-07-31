package repoaudit

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEvidenceRootsRejectContainmentAliasAndIdentityDrift(t *testing.T) {
	t.Parallel()

	config := newEvidenceTestConfig(t)
	contained := config
	contained.TaskDir = config.ControlRoot
	if _, err := OpenEvidenceOwner(contained); err == nil {
		t.Fatal("contained evidence/task roots were accepted")
	}

	alias := filepath.Join(filepath.Dir(config.Workspace), "workspace-alias")
	if err := os.Symlink(config.Workspace, alias); err == nil {
		aliased := config
		aliased.Workspace = alias
		if _, err := OpenEvidenceOwner(aliased); err == nil {
			t.Fatal("symlink-aliased workspace was accepted")
		}
	}

	owner, err := OpenEvidenceOwner(config)
	if err != nil {
		t.Fatalf("OpenEvidenceOwner: %v", err)
	}
	moved := config.ControlRoot + "-moved"
	if err := os.Rename(config.ControlRoot, moved); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission) {
			if err := owner.Publish("identity.json", []byte("{}\n")); err != nil {
				owner.Close()
				t.Fatalf("owner unusable after Windows blocked control-root replacement: %v", err)
			}
			if err := owner.Close(); err != nil {
				t.Fatal(err)
			}
			t.Log("Windows blocked control-root replacement while the lease was held")
			return
		}
		owner.Close()
		t.Fatal(err)
	}
	if err := os.Mkdir(config.ControlRoot, 0o700); err != nil {
		owner.Close()
		t.Fatal(err)
	}
	if err := owner.Publish("identity.json", []byte("{}\n")); err == nil {
		owner.Close()
		t.Fatal("control-root identity drift was not detected")
	}
	if err := owner.Close(); err == nil {
		t.Fatal("Close did not report control-root identity drift")
	}
}

func TestEvidenceBootstrapCrashMatrixIsDurable(t *testing.T) {
	t.Parallel()

	for _, state := range []bootstrapState{bootstrapA, bootstrapB, bootstrapC, bootstrapD} {
		state := state
		t.Run(bootstrapStateName(state), func(t *testing.T) {
			t.Parallel()
			config := newEvidenceTestConfig(t)
			prepareEvidenceBootstrapState(t, config, state)
			owner, err := OpenEvidenceOwner(config)
			if err != nil {
				t.Fatalf("OpenEvidenceOwner(%s): %v", bootstrapStateName(state), err)
			}
			if err := owner.Close(); err != nil {
				t.Fatalf("Close(%s): %v", bootstrapStateName(state), err)
			}
			got, err := inspectBootstrapState(config)
			if err != nil {
				t.Fatal(err)
			}
			if got != bootstrapD {
				t.Fatalf("bootstrap state = %v, want D", got)
			}
		})
	}
}

func TestAtomicPublisherRecoversOnlyClosedStates(t *testing.T) {
	t.Parallel()

	config := newEvidenceTestConfig(t)
	owner, err := OpenEvidenceOwner(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.EnsureDir("preservation"); err != nil {
		t.Fatal(err)
	}
	expected := []byte("canonical evidence\n")
	if err := owner.Publish("preservation/result.json", expected); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := owner.Publish("preservation/result.json", expected); err != nil {
		t.Fatalf("idempotent Publish: %v", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	parent := filepath.Join(config.EvidenceDir, "preservation")
	temp := filepath.Join(parent, ".recovered.json.publish.tmp")
	writePrivateBytes(t, temp, 0o600, expected[:8])
	owner, err = OpenEvidenceOwner(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Publish("preservation/recovered.json", expected); err != nil {
		t.Fatalf("recover truncated temp: %v", err)
	}
	badTemp := filepath.Join(parent, ".bad.json.publish.tmp")
	writePrivateBytes(t, badTemp, 0o600, []byte("different complete bytes"))
	if err := owner.Publish("preservation/bad.json", expected); err == nil {
		t.Fatal("publisher accepted an inconsistent temp")
	}
	badFinal := filepath.Join(parent, "wrong.json")
	writePrivateBytes(t, badFinal, 0o600, []byte("wrong"))
	if err := owner.Publish("preservation/wrong.json", expected); err == nil {
		t.Fatal("publisher accepted an inconsistent final")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMissingLeaseWithExistingLedgerFailsClosed(t *testing.T) {
	t.Parallel()

	for _, active := range []bool{false, true} {
		config := newEvidenceTestConfig(t)
		if err := os.Mkdir(config.EvidenceDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if active {
			writePrivateFile(t, filepath.Join(config.ControlRoot, evidenceActiveName), 0o400)
		}
		if _, err := OpenEvidenceOwner(config); err == nil {
			t.Fatalf("missing lease with ledger (active=%v) was accepted", active)
		}
	}
}

func newEvidenceTestConfig(t *testing.T) EvidenceConfig {
	t.Helper()
	base := t.TempDir()
	paths := map[string]string{
		"control":   filepath.Join(base, "control"),
		"task":      filepath.Join(base, "task"),
		"workspace": filepath.Join(base, "workspace"),
	}
	for name, path := range paths {
		var err error
		if name == "control" {
			err = createPrivateDirectory(path, 0o700)
		} else {
			err = os.Mkdir(path, 0o700)
		}
		if err != nil {
			t.Fatal(err)
		}
		if name != "control" {
			if err := os.Chmod(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	source := filepath.Join(paths["task"], "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o700); err != nil {
		t.Fatal(err)
	}
	return EvidenceConfig{
		ControlRoot: paths["control"],
		EvidenceDir: filepath.Join(paths["control"], "evidence"),
		TaskDir:     paths["task"],
		Workspace:   paths["workspace"],
		SourceRoot:  source,
	}
}

func prepareEvidenceBootstrapState(t *testing.T, config EvidenceConfig, state bootstrapState) {
	t.Helper()
	if state >= bootstrapB {
		writePrivateFile(t, filepath.Join(config.ControlRoot, evidenceLeaseName), 0o600)
	}
	if state >= bootstrapC {
		if err := createPrivateDirectory(config.EvidenceDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if state >= bootstrapD {
		writePrivateFile(t, filepath.Join(config.ControlRoot, evidenceActiveName), 0o400)
	}
}

func writePrivateFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	file, err := createPrivateFile(path, mode, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writePrivateBytes(t *testing.T, path string, mode os.FileMode, data []byte) {
	t.Helper()
	file, err := createPrivateFile(path, mode, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func bootstrapStateName(state bootstrapState) string {
	return []string{"A", "B", "C", "D"}[state]
}

var _ = errors.Is

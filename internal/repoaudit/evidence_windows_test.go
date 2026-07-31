//go:build windows

package repoaudit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const evidenceWindowsHelperEnv = "XAGENT_EVIDENCE_WINDOWS_HELPER"

func TestEvidenceLeaseSerializesProcessesAndIsNotInherited(t *testing.T) {
	config := newEvidenceTestConfig(t)
	first, err := OpenEvidenceOwner(config)
	if err != nil {
		t.Fatal(err)
	}

	flags, err := windowsHandleFlags(windows.Handle(first.lease.Fd()))
	if err != nil {
		first.Close()
		t.Fatal(err)
	}
	if flags&windows.HANDLE_FLAG_INHERIT != 0 {
		first.Close()
		t.Fatal("evidence lease handle is inheritable")
	}

	marker := filepath.Join(config.TaskDir, "second-owner-entered")
	command := exec.Command(os.Args[0], "-test.run=^TestEvidenceWindowsLeaseHelper$", "-test.count=1")
	command.Env = append(os.Environ(),
		evidenceWindowsHelperEnv+"=1",
		"XAGENT_EVIDENCE_WINDOWS_CONTROL="+config.ControlRoot,
		"XAGENT_EVIDENCE_WINDOWS_DIR="+config.EvidenceDir,
		"XAGENT_EVIDENCE_WINDOWS_TASK="+config.TaskDir,
		"XAGENT_EVIDENCE_WINDOWS_WORKSPACE="+config.Workspace,
		"XAGENT_EVIDENCE_WINDOWS_SOURCE="+config.SourceRoot,
		"XAGENT_EVIDENCE_WINDOWS_MARKER="+marker,
		"XAGENT_EVIDENCE_WINDOWS_HANDLE="+strconv.FormatUint(uint64(first.lease.Fd()), 10),
	)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		first.Close()
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	if _, err := os.Lstat(marker); err == nil {
		first.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("second evidence owner entered before [0,1) lease release")
	} else if !errors.Is(err, os.ErrNotExist) {
		first.Close()
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}

	if err := first.Close(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Windows lease helper failed: %v\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		<-done
		t.Fatal("second evidence owner did not enter after [0,1) lease release")
	}
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("second evidence owner did not publish its entry marker: %v", err)
	}
}

func TestEvidenceWindowsLeaseHelper(t *testing.T) {
	if os.Getenv(evidenceWindowsHelperEnv) != "1" {
		return
	}
	rawHandle, err := strconv.ParseUint(os.Getenv("XAGENT_EVIDENCE_WINDOWS_HANDLE"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := windowsHandleFlags(windows.Handle(rawHandle)); err == nil {
		t.Fatal("child inherited the evidence lease handle")
	} else if !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
		t.Fatalf("query child lease handle: %v", err)
	}
	config := EvidenceConfig{
		ControlRoot: os.Getenv("XAGENT_EVIDENCE_WINDOWS_CONTROL"),
		EvidenceDir: os.Getenv("XAGENT_EVIDENCE_WINDOWS_DIR"),
		TaskDir:     os.Getenv("XAGENT_EVIDENCE_WINDOWS_TASK"),
		Workspace:   os.Getenv("XAGENT_EVIDENCE_WINDOWS_WORKSPACE"),
		SourceRoot:  os.Getenv("XAGENT_EVIDENCE_WINDOWS_SOURCE"),
	}
	owner, err := OpenEvidenceOwner(config)
	if err != nil {
		t.Fatal(err)
	}
	marker := os.Getenv("XAGENT_EVIDENCE_WINDOWS_MARKER")
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		owner.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		owner.Close()
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceDACLAndModesAreExact(t *testing.T) {
	config := newEvidenceTestConfig(t)
	owner, err := OpenEvidenceOwner(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.EnsureDir("preservation"); err != nil {
		owner.Close()
		t.Fatal(err)
	}
	if err := owner.Publish("preservation/result.json", []byte("{}\n")); err != nil {
		owner.Close()
		t.Fatal(err)
	}
	checks := []struct {
		path      string
		directory bool
		empty     bool
	}{
		{path: config.ControlRoot, directory: true},
		{path: config.EvidenceDir, directory: true},
		{path: filepath.Join(config.EvidenceDir, "preservation"), directory: true},
		{path: filepath.Join(config.ControlRoot, evidenceLeaseName)},
		{path: filepath.Join(config.ControlRoot, evidenceActiveName), empty: true},
		{path: filepath.Join(config.EvidenceDir, "preservation", "result.json")},
	}
	for _, check := range checks {
		if check.directory {
			err = validatePrivateDirectory(check.path, 0)
		} else {
			err = validatePrivateFile(check.path, 0, check.empty)
		}
		if err != nil {
			owner.Close()
			t.Fatalf("private DACL %s: %v", filepath.Base(check.path), err)
		}
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	tampered := filepath.Join(config.EvidenceDir, "preservation", "result.json")
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{privateWindowsAccessEntry(world, windows.TRUSTEE_IS_WELL_KNOWN_GROUP)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(tampered, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateFile(tampered, 0, false); err == nil {
		t.Fatal("private DACL validation accepted an Everyone-only ACL")
	}
}

func windowsHandleFlags(handle windows.Handle) (uint32, error) {
	procedure := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetHandleInformation")
	var flags uint32
	result, _, callErr := procedure.Call(uintptr(handle), uintptr(unsafe.Pointer(&flags)))
	if result == 0 {
		if callErr != nil && callErr != windows.ERROR_SUCCESS {
			return 0, callErr
		}
		return 0, fmt.Errorf("GetHandleInformation failed")
	}
	return flags, nil
}

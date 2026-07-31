package repoaudit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	evidenceLeaseName  = ".xagent-evidence.lease"
	evidenceActiveName = ".xagent-evidence.active"
)

// EvidenceConfig fixes all roots before any ledger content is observed.
type EvidenceConfig struct {
	ControlRoot string
	EvidenceDir string
	TaskDir     string
	Workspace   string
	SourceRoot  string
}

// EvidenceOwner holds the one cross-process lease for a ledger operation.
type EvidenceOwner struct {
	config       EvidenceConfig
	lease        *os.File
	controlInfo  os.FileInfo
	evidenceInfo os.FileInfo
	mu           sync.Mutex
	closed       bool
}

// EnsureEvidenceControlRoot creates the one private control root when absent,
// or validates the existing root without weakening its permissions.
func EnsureEvidenceControlRoot(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("evidence control root must be a canonical absolute path")
	}
	err := createPrivateDirectory(path, 0o700)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create evidence control root: %w", err)
	}
	if err := validatePrivateDirectory(path, 0o700); err != nil {
		return fmt.Errorf("validate evidence control root: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync evidence control parent: %w", err)
	}
	return nil
}

func OpenEvidenceOwner(config EvidenceConfig) (*EvidenceOwner, error) {
	normalized, err := normalizeEvidenceConfig(config)
	if err != nil {
		return nil, err
	}
	if err := validatePrivateDirectory(normalized.ControlRoot, 0o700); err != nil {
		return nil, fmt.Errorf("validate evidence control root: %w", err)
	}
	controlInfo, err := os.Lstat(normalized.ControlRoot)
	if err != nil {
		return nil, fmt.Errorf("stat evidence control root: %w", err)
	}

	state, err := inspectBootstrapState(normalized)
	if err != nil {
		return nil, err
	}
	if state == bootstrapA {
		leasePath := filepath.Join(normalized.ControlRoot, evidenceLeaseName)
		file, createErr := createPrivateFile(leasePath, 0o600, true)
		if createErr != nil && !errors.Is(createErr, fs.ErrExist) {
			return nil, fmt.Errorf("create evidence lease: %w", createErr)
		}
		if createErr == nil {
			if err := file.Sync(); err != nil {
				file.Close()
				return nil, fmt.Errorf("sync evidence lease: %w", err)
			}
			if err := file.Close(); err != nil {
				return nil, fmt.Errorf("close new evidence lease: %w", err)
			}
			if err := syncDirectory(normalized.ControlRoot); err != nil {
				return nil, fmt.Errorf("sync evidence control root: %w", err)
			}
		}
	}

	leasePath := filepath.Join(normalized.ControlRoot, evidenceLeaseName)
	lease, err := openLeaseNoFollow(leasePath)
	if err != nil {
		return nil, err
	}
	if err := lockEvidenceLease(lease); err != nil {
		lease.Close()
		return nil, err
	}
	owner := &EvidenceOwner{config: normalized, lease: lease, controlInfo: controlInfo}
	cleanup := true
	defer func() {
		if cleanup {
			_ = owner.Close()
		}
	}()

	if err := lease.Sync(); err != nil {
		return nil, fmt.Errorf("sync locked evidence lease: %w", err)
	}
	if err := syncDirectory(normalized.ControlRoot); err != nil {
		return nil, fmt.Errorf("sync locked evidence control root: %w", err)
	}
	if err := owner.verifyControlIdentity(); err != nil {
		return nil, err
	}
	state, err = inspectBootstrapState(normalized)
	if err != nil {
		return nil, err
	}
	if state == bootstrapB {
		if err := createPrivateDirectory(normalized.EvidenceDir, 0o700); err != nil {
			return nil, fmt.Errorf("create evidence directory: %w", err)
		}
		if err := syncDirectory(normalized.EvidenceDir); err != nil {
			return nil, fmt.Errorf("sync new evidence directory: %w", err)
		}
		if err := syncDirectory(normalized.ControlRoot); err != nil {
			return nil, fmt.Errorf("sync evidence directory entry: %w", err)
		}
		state = bootstrapC
	}
	if state == bootstrapC {
		if err := requireEmptyDirectory(normalized.EvidenceDir); err != nil {
			return nil, err
		}
		if err := syncDirectory(normalized.EvidenceDir); err != nil {
			return nil, fmt.Errorf("sync empty evidence directory: %w", err)
		}
		activePath := filepath.Join(normalized.ControlRoot, evidenceActiveName)
		active, err := createPrivateFile(activePath, 0o400, false)
		if err != nil {
			return nil, fmt.Errorf("create evidence active sentinel: %w", err)
		}
		if err := active.Sync(); err != nil {
			active.Close()
			return nil, fmt.Errorf("sync evidence active sentinel: %w", err)
		}
		if err := active.Close(); err != nil {
			return nil, fmt.Errorf("close evidence active sentinel: %w", err)
		}
		if err := syncDirectory(normalized.ControlRoot); err != nil {
			return nil, fmt.Errorf("sync evidence active entry: %w", err)
		}
	}
	if state, err = inspectBootstrapState(normalized); err != nil || state != bootstrapD {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("evidence bootstrap did not reach stable state D")
	}
	if err := validatePrivateFile(filepath.Join(normalized.ControlRoot, evidenceLeaseName), 0o600, false); err != nil {
		return nil, err
	}
	if err := validatePrivateFile(filepath.Join(normalized.ControlRoot, evidenceActiveName), 0o400, true); err != nil {
		return nil, err
	}
	if err := validatePrivateDirectory(normalized.EvidenceDir, 0o700); err != nil {
		return nil, err
	}
	owner.evidenceInfo, err = os.Lstat(normalized.EvidenceDir)
	if err != nil {
		return nil, err
	}
	cleanup = false
	return owner, nil
}

// EnsureDir creates one private ledger directory beneath the evidence root.
func (o *EvidenceOwner) EnsureDir(relative string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.verifyUsable(); err != nil {
		return err
	}
	clean, err := cleanEvidenceRelative(relative)
	if err != nil {
		return err
	}
	path := filepath.Join(o.config.EvidenceDir, clean)
	if err := createPrivateDirectory(path, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create evidence child directory: %w", err)
	}
	if err := validatePrivateDirectory(path, 0o700); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// Read returns one complete private ledger file while the owner lease is held.
func (o *EvidenceOwner) Read(relative string) ([]byte, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.verifyUsable(); err != nil {
		return nil, err
	}
	clean, err := cleanEvidenceRelative(relative)
	if err != nil {
		return nil, err
	}
	data, exists, err := readPublishFile(filepath.Join(o.config.EvidenceDir, clean), 0o600)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

// Publish atomically publishes expected bytes without replacing a final file.
func (o *EvidenceOwner) Publish(relative string, expected []byte) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.verifyUsable(); err != nil {
		return err
	}
	clean, err := cleanEvidenceRelative(relative)
	if err != nil {
		return err
	}
	finalPath := filepath.Join(o.config.EvidenceDir, clean)
	parent := filepath.Dir(finalPath)
	if err := validatePrivateDirectory(parent, 0o700); err != nil {
		return err
	}
	tempPath := filepath.Join(parent, "."+filepath.Base(finalPath)+".publish.tmp")

	finalBytes, finalState, err := readPublishFile(finalPath, 0o600)
	if err != nil {
		return err
	}
	tempBytes, tempState, err := readPublishFile(tempPath, 0o600)
	if err != nil {
		return err
	}
	if finalState {
		if !bytes.Equal(finalBytes, expected) {
			return errors.New("existing evidence final differs from expected bytes")
		}
		if tempState {
			if !bytes.Equal(tempBytes, expected) && !bytes.HasPrefix(expected, tempBytes) {
				return errors.New("evidence final has an inconsistent publish temp")
			}
			if err := os.Remove(tempPath); err != nil {
				return fmt.Errorf("remove recovered evidence temp: %w", err)
			}
			if err := syncDirectory(parent); err != nil {
				return err
			}
		}
		return nil
	}
	if tempState && !bytes.Equal(tempBytes, expected) {
		if !bytes.HasPrefix(expected, tempBytes) {
			return errors.New("existing evidence temp differs from expected bytes")
		}
		if err := os.Remove(tempPath); err != nil {
			return fmt.Errorf("remove truncated evidence temp: %w", err)
		}
		tempState = false
	}
	if !tempState {
		temp, err := createPrivateFile(tempPath, 0o600, false)
		if err != nil {
			return fmt.Errorf("create evidence publish temp: %w", err)
		}
		if _, err := temp.Write(expected); err != nil {
			temp.Close()
			return fmt.Errorf("write evidence publish temp: %w", err)
		}
		if err := temp.Sync(); err != nil {
			temp.Close()
			return fmt.Errorf("sync evidence publish temp: %w", err)
		}
		if err := temp.Close(); err != nil {
			return fmt.Errorf("close evidence publish temp: %w", err)
		}
	}
	verified, exists, err := readPublishFile(tempPath, 0o600)
	if err != nil || !exists || !bytes.Equal(verified, expected) {
		if err != nil {
			return err
		}
		return errors.New("evidence publish temp failed readback")
	}
	if err := os.Link(tempPath, finalPath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errors.New("evidence final appeared during no-replace publish")
		}
		return fmt.Errorf("no-replace evidence publish: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return fmt.Errorf("remove published evidence temp: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("sync evidence publish parent: %w", err)
	}
	return nil
}

func (o *EvidenceOwner) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	var errs []error
	if o.lease != nil {
		if err := o.verifyControlIdentity(); err != nil {
			errs = append(errs, err)
		}
		if err := syncDirectory(o.config.ControlRoot); err != nil {
			errs = append(errs, err)
		}
		if err := unlockEvidenceLease(o.lease); err != nil {
			errs = append(errs, err)
		}
		if err := o.lease.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (o *EvidenceOwner) verifyUsable() error {
	if o.closed {
		return errors.New("evidence owner is closed")
	}
	if err := o.verifyControlIdentity(); err != nil {
		return err
	}
	current, err := os.Lstat(o.config.EvidenceDir)
	if err != nil {
		return fmt.Errorf("stat evidence directory: %w", err)
	}
	if !os.SameFile(current, o.evidenceInfo) {
		return errors.New("evidence directory identity changed")
	}
	state, err := inspectBootstrapState(o.config)
	if err != nil {
		return err
	}
	if state != bootstrapD {
		return errors.New("evidence bootstrap state changed")
	}
	return nil
}

func (o *EvidenceOwner) verifyControlIdentity() error {
	current, err := os.Lstat(o.config.ControlRoot)
	if err != nil {
		return fmt.Errorf("stat evidence control root: %w", err)
	}
	if !os.SameFile(current, o.controlInfo) {
		return errors.New("evidence control root identity changed")
	}
	return validatePrivateDirectory(o.config.ControlRoot, 0o700)
}

type bootstrapState int

const (
	bootstrapA bootstrapState = iota
	bootstrapB
	bootstrapC
	bootstrapD
)

func inspectBootstrapState(config EvidenceConfig) (bootstrapState, error) {
	entries, err := os.ReadDir(config.ControlRoot)
	if err != nil {
		return 0, fmt.Errorf("enumerate evidence control root: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	evidenceName := filepath.Base(config.EvidenceDir)
	allowed := map[string]bool{evidenceLeaseName: true, evidenceName: true, evidenceActiveName: true}
	for _, name := range names {
		if !allowed[name] {
			return 0, errors.New("evidence control root contains an extra child")
		}
	}
	hasLease, hasDir, hasActive := containsName(names, evidenceLeaseName), containsName(names, evidenceName), containsName(names, evidenceActiveName)
	switch {
	case !hasLease && !hasDir && !hasActive:
		return bootstrapA, nil
	case hasLease && !hasDir && !hasActive:
		return bootstrapB, nil
	case hasLease && hasDir && !hasActive:
		if err := requireEmptyDirectory(config.EvidenceDir); err != nil {
			return 0, err
		}
		return bootstrapC, nil
	case hasLease && hasDir && hasActive:
		return bootstrapD, nil
	default:
		return 0, errors.New("evidence control root is in an unrecoverable bootstrap state")
	}
}

func normalizeEvidenceConfig(config EvidenceConfig) (EvidenceConfig, error) {
	fields := []*string{&config.ControlRoot, &config.EvidenceDir, &config.TaskDir, &config.Workspace, &config.SourceRoot}
	for _, field := range fields {
		if *field == "" {
			return EvidenceConfig{}, errors.New("evidence roots must be non-empty absolute paths")
		}
		absolute, err := filepath.Abs(*field)
		if err != nil {
			return EvidenceConfig{}, err
		}
		if absolute != filepath.Clean(*field) {
			return EvidenceConfig{}, errors.New("evidence roots must already be canonical absolute paths")
		}
		*field = absolute
	}
	evidenceBase := filepath.Base(config.EvidenceDir)
	existingFields := []*string{&config.ControlRoot, &config.TaskDir, &config.Workspace, &config.SourceRoot}
	for _, existing := range existingFields {
		info, err := os.Lstat(*existing)
		if err != nil {
			return EvidenceConfig{}, fmt.Errorf("stat evidence-related root: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return EvidenceConfig{}, errors.New("evidence-related roots must not be symlinks")
		}
		resolved, err := filepath.EvalSymlinks(*existing)
		if err != nil {
			return EvidenceConfig{}, fmt.Errorf("resolve evidence-related root: %w", err)
		}
		*existing = resolved
	}
	config.EvidenceDir = filepath.Join(config.ControlRoot, evidenceBase)
	if filepath.Dir(config.EvidenceDir) != config.ControlRoot {
		return EvidenceConfig{}, errors.New("evidence directory must be a direct child of its control root")
	}
	if filepath.Base(config.EvidenceDir) == evidenceLeaseName || filepath.Base(config.EvidenceDir) == evidenceActiveName {
		return EvidenceConfig{}, errors.New("evidence directory name conflicts with control files")
	}
	for _, other := range []string{config.TaskDir, config.Workspace, config.SourceRoot} {
		if pathsContainOrAlias(config.EvidenceDir, other) {
			return EvidenceConfig{}, errors.New("evidence root must not contain or alias task, workspace or source roots")
		}
	}
	return config, nil
}

func pathsContainOrAlias(first, second string) bool {
	if strings.EqualFold(first, second) {
		return true
	}
	separator := string(filepath.Separator)
	return strings.HasPrefix(strings.ToLower(first)+separator, strings.ToLower(second)+separator) || strings.HasPrefix(strings.ToLower(second)+separator, strings.ToLower(first)+separator)
}

func cleanEvidenceRelative(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, 0) || strings.ContainsRune(value, '\\') || path.IsAbs(value) || path.Clean(value) != value || value == "." || value == ".." || strings.HasPrefix(value, "../") {
		return "", errors.New("evidence path must be a normalized relative path")
	}
	converted := filepath.FromSlash(value)
	if filepath.IsAbs(converted) || filepath.VolumeName(converted) != "" {
		return "", errors.New("evidence path must be a normalized relative path")
	}
	return converted, nil
}

func containsName(names []string, name string) bool {
	index := sort.SearchStrings(names, name)
	return index < len(names) && names[index] == name
}

func requireEmptyDirectory(path string) error {
	if err := validatePrivateDirectory(path, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("bootstrap evidence directory must be empty before active sentinel")
	}
	return nil
}

func openLeaseNoFollow(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("lstat evidence lease: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		return nil, errors.New("evidence lease must be an empty regular file")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open evidence lease: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, errors.New("evidence lease identity changed while opening")
	}
	return file, nil
}

func readPublishFile(path string, mode os.FileMode) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("evidence publish target has invalid type or mode")
	}
	if err := validatePrivateFile(path, mode, false); err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

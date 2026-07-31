package repoaudit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// SourceKind selects one explicit repository view.
type SourceKind string

const (
	SourceIndex    SourceKind = "index"
	SourceCommit   SourceKind = "commit"
	SourceWorktree SourceKind = "worktree"
)

// Entry is a NUL-safe Git entry. Path may contain any byte except NUL.
type Entry struct {
	Path  string
	Mode  uint32
	OID   string
	Stage int
	Type  string
}

// Source exposes a deterministic entry list and entry content.
type Source interface {
	Entries(context.Context) ([]Entry, error)
	Open(context.Context, Entry) (io.ReadCloser, error)
}

// GitSource reads one explicit Git or worktree view.
type GitSource struct {
	Repo     string
	Kind     SourceKind
	Revision string
}

func NewIndexSource(repo string) GitSource {
	return GitSource{Repo: repo, Kind: SourceIndex}
}

func NewCommitSource(repo, revision string) GitSource {
	return GitSource{Repo: repo, Kind: SourceCommit, Revision: revision}
}

func NewWorktreeSource(repo string) GitSource {
	return GitSource{Repo: repo, Kind: SourceWorktree}
}

// Entries returns entries using only NUL-delimited Git output.
func (s GitSource) Entries(ctx context.Context) ([]Entry, error) {
	switch s.Kind {
	case SourceIndex:
		data, err := s.gitOutput(ctx, "ls-files", "--stage", "-z")
		if err != nil {
			return nil, err
		}
		return parseIndexEntries(data)
	case SourceCommit:
		if !isLowerGitOID(s.Revision) {
			return nil, errors.New("commit source requires a canonical lowercase object ID")
		}
		data, err := s.gitOutput(ctx, "ls-tree", "-rz", "--full-tree", s.Revision)
		if err != nil {
			return nil, err
		}
		return parseTreeEntries(data)
	case SourceWorktree:
		data, err := s.gitOutput(ctx, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		if err != nil {
			return nil, err
		}
		return s.parseWorktreeEntries(data)
	default:
		return nil, fmt.Errorf("unsupported repository source %q", s.Kind)
	}
}

// Open opens exact content from the selected source. Git content is addressed
// by object ID; worktree content is opened beneath an os.Root.
func (s GitSource) Open(ctx context.Context, entry Entry) (io.ReadCloser, error) {
	switch s.Kind {
	case SourceIndex, SourceCommit:
		if entry.Type == "commit" || entry.Mode == 0160000 {
			return nil, errors.New("gitlink has no blob content")
		}
		data, err := s.gitOutput(ctx, "cat-file", "blob", entry.OID)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	case SourceWorktree:
		root, err := os.OpenRoot(s.Repo)
		if err != nil {
			return nil, fmt.Errorf("open worktree root: %w", err)
		}
		file, err := root.Open(filepath.ToSlash(entry.Path))
		if err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("open worktree entry: %w", err)
		}
		return &rootedFile{File: file, root: root}, nil
	default:
		return nil, fmt.Errorf("unsupported repository source %q", s.Kind)
	}
}

type rootedFile struct {
	*os.File
	root *os.Root
}

func (f *rootedFile) Close() error {
	fileErr := f.File.Close()
	rootErr := f.root.Close()
	return errors.Join(fileErr, rootErr)
}

func (s GitSource) gitOutput(ctx context.Context, args ...string) ([]byte, error) {
	commandArgs := append([]string{"-C", s.Repo, "--no-pager", "--no-replace-objects"}, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1")
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("git command failed with exit code %d", exitErr.ExitCode())
		}
		return nil, fmt.Errorf("run git command: %w", err)
	}
	return output, nil
}

func parseIndexEntries(data []byte) ([]Entry, error) {
	records, err := splitNULRecords(data)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(records))
	for _, record := range records {
		tab := bytes.IndexByte(record, '\t')
		if tab <= 0 || tab == len(record)-1 {
			return nil, errors.New("invalid NUL-delimited index record")
		}
		fields := strings.Split(string(record[:tab]), " ")
		if len(fields) != 3 {
			return nil, errors.New("invalid index metadata field count")
		}
		mode, err := parseGitMode(fields[0])
		if err != nil {
			return nil, err
		}
		if !isLowerGitOID(fields[1]) {
			return nil, errors.New("invalid index object ID")
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil || stage < 0 || stage > 3 {
			return nil, errors.New("invalid index stage")
		}
		entries = append(entries, Entry{Path: string(record[tab+1:]), Mode: mode, OID: fields[1], Stage: stage, Type: gitTypeForMode(mode)})
	}
	return entries, nil
}

func parseTreeEntries(data []byte) ([]Entry, error) {
	records, err := splitNULRecords(data)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(records))
	for _, record := range records {
		tab := bytes.IndexByte(record, '\t')
		if tab <= 0 || tab == len(record)-1 {
			return nil, errors.New("invalid NUL-delimited tree record")
		}
		fields := strings.Split(string(record[:tab]), " ")
		if len(fields) != 3 {
			return nil, errors.New("invalid tree metadata field count")
		}
		mode, err := parseGitMode(fields[0])
		if err != nil {
			return nil, err
		}
		if !isLowerGitOID(fields[2]) {
			return nil, errors.New("invalid tree object ID")
		}
		entries = append(entries, Entry{Path: string(record[tab+1:]), Mode: mode, OID: fields[2], Stage: 0, Type: fields[1]})
	}
	return entries, nil
}

func (s GitSource) parseWorktreeEntries(data []byte) ([]Entry, error) {
	records, err := splitNULRecords(data)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(s.Repo)
	if err != nil {
		return nil, fmt.Errorf("open worktree root: %w", err)
	}
	defer root.Close()
	entries := make([]Entry, 0, len(records))
	for _, record := range records {
		name := string(record)
		info, err := root.Lstat(filepath.ToSlash(name))
		if err != nil {
			return nil, fmt.Errorf("lstat worktree entry: %w", err)
		}
		entries = append(entries, Entry{Path: name, Mode: uint32(info.Mode()), Stage: 0, Type: fileTypeName(info.Mode())})
	}
	return entries, nil
}

func splitNULRecords(data []byte) ([][]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, errors.New("Git output is not NUL terminated")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	for _, part := range parts {
		if len(part) == 0 {
			return nil, errors.New("Git output contains an empty NUL record")
		}
	}
	return parts, nil
}

func parseGitMode(value string) (uint32, error) {
	mode, err := strconv.ParseUint(value, 8, 32)
	if err != nil {
		return 0, errors.New("invalid Git mode")
	}
	return uint32(mode), nil
}

func isLowerGitOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func gitTypeForMode(mode uint32) string {
	if mode == 0160000 {
		return "commit"
	}
	return "blob"
}

func fileTypeName(mode os.FileMode) string {
	switch {
	case mode.IsRegular():
		return "regular"
	case mode.IsDir():
		return "directory"
	case mode&os.ModeSymlink != 0:
		return "symlink"
	default:
		return "other"
	}
}

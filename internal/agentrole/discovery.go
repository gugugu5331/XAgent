package agentrole

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

type discoveredCandidate struct {
	origin    string
	candidate Candidate
}

func DiscoverFiles(ctx context.Context, source FileSource, limits Limits, redactor *redact.RuntimeRedactor) ([]Candidate, error) {
	if ctx == nil || redactor == nil || limits.Validate() != nil {
		return nil, safeDiscoveryError(redactor, "role_source_invalid", "role source configuration is invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source.Source != SourceBuiltin && source.Source != SourceUser && source.Source != SourceProject {
		return nil, safeDiscoveryError(redactor, "role_source_invalid", "role file source tier is invalid")
	}
	if _, err := normalizeIdentifier(source.ID, limits.MaxSourceIDBytes); err != nil {
		return nil, safeDiscoveryError(redactor, "role_source_invalid", "role file source identity is invalid")
	}
	if source.FS == nil || source.Root == "" || int64(len(source.Root)) > limits.MaxRootBytes || !fs.ValidPath(source.Root) {
		return nil, safeDiscoveryError(redactor, "role_source_invalid", "role file source root is invalid")
	}
	if linkFS, ok := source.FS.(fs.ReadLinkFS); ok {
		info, err := lstatRootWithoutSymlinks(linkFS, source.Root)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && source.Source != SourceBuiltin {
				return nil, nil
			}
			return nil, safeDiscoveryError(redactor, "role_source_unavailable", "role file source root is unavailable")
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, safeDiscoveryError(redactor, "role_source_unsafe", "role file source root is a symbolic link")
		}
	}

	directory, err := source.FS.Open(source.Root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && source.Source != SourceBuiltin {
			return nil, nil
		}
		return nil, safeDiscoveryError(redactor, "role_source_unavailable", "role file source root is unavailable")
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		return nil, safeDiscoveryError(redactor, "role_source_invalid", "role file source root is not a directory")
	}
	reader, ok := directory.(fs.ReadDirFile)
	if !ok {
		return nil, safeDiscoveryError(redactor, "role_source_invalid", "role file source cannot enumerate entries")
	}

	entriesSeen := 0
	candidatesSeen := 0
	var totalBytes int64
	discovered := make([]discoveredCandidate, 0)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, readErr := reader.ReadDir(1)
		if len(entries) > 0 {
			entriesSeen++
			if entriesSeen > limits.MaxFiles {
				return nil, safeDiscoveryError(redactor, "role_source_limit", "role file source exceeds its entry limit")
			}
			entry := entries[0]
			name := entry.Name()
			if shouldLoadRoleEntry(name, entry) {
				candidatesSeen++
				if candidatesSeen > limits.MaxCandidates {
					return nil, safeDiscoveryError(redactor, "role_source_limit", "role file source exceeds its candidate limit")
				}
				origin := path.Join(source.Root, name)
				candidate, bytesRead := discoverRoleEntry(ctx, source.FS, origin, entry, limits, redactor)
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if bytesRead > limits.MaxTotalBytes-totalBytes {
					return nil, safeDiscoveryError(redactor, "role_source_limit", "role file source exceeds its total byte limit")
				}
				totalBytes += bytesRead
				discovered = append(discovered, discoveredCandidate{origin: origin, candidate: candidate})
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, safeDiscoveryError(redactor, "role_source_failed", "role file source enumeration failed")
		}
	}

	sort.Slice(discovered, func(i, j int) bool { return discovered[i].origin < discovered[j].origin })
	result := make([]Candidate, len(discovered))
	for index := range discovered {
		result[index] = discovered[index].candidate
	}
	return result, nil
}

func shouldLoadRoleEntry(name string, entry fs.DirEntry) bool {
	if name == "" || strings.HasPrefix(name, ".") || name != strings.ToLower(name) || !strings.HasSuffix(name, ".md") {
		return false
	}
	return !entry.IsDir()
}

func discoverRoleEntry(
	ctx context.Context,
	fileSystem fs.FS,
	origin string,
	entry fs.DirEntry,
	limits Limits,
	redactor *redact.RuntimeRedactor,
) (Candidate, int64) {
	safeOrigin := redactor.Redact(origin)
	invalid := func(code, message string) (Candidate, int64) {
		return invalidCandidate(safeOrigin, redactor, code, message), 0
	}
	if entry.Type()&fs.ModeSymlink != 0 {
		return invalid("role_entry_unsafe", "role entry is a symbolic link")
	}
	info, err := entry.Info()
	if err != nil || !info.Mode().IsRegular() {
		return invalid("role_entry_invalid", "role entry is not a regular file")
	}
	if info.Size() < 0 || info.Size() > limits.MaxEntryBytes {
		return invalid("role_entry_limit", "role entry exceeds its size limit")
	}
	var lstatInfo fs.FileInfo
	if linkFS, ok := fileSystem.(fs.ReadLinkFS); ok {
		lstatInfo, err = linkFS.Lstat(origin)
		if err != nil || lstatInfo.Mode()&fs.ModeSymlink != 0 || !lstatInfo.Mode().IsRegular() {
			return invalid("role_entry_unsafe", "role entry changed during discovery")
		}
	}
	file, err := fileSystem.Open(origin)
	if err != nil {
		return invalid("role_entry_unreadable", "role entry cannot be read")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || openedInfo.Mode()&fs.ModeSymlink != 0 {
		return invalid("role_entry_unsafe", "role entry changed during discovery")
	}
	if lstatInfo != nil && lstatInfo.Sys() != nil && openedInfo.Sys() != nil && !os.SameFile(lstatInfo, openedInfo) {
		return invalid("role_entry_unsafe", "role entry identity changed during discovery")
	}
	if err := ctx.Err(); err != nil {
		return invalid("role_entry_cancelled", "role entry read was cancelled")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limits.MaxEntryBytes+1))
	if err != nil {
		return invalid("role_entry_unreadable", "role entry cannot be read")
	}
	if int64(len(raw)) > limits.MaxEntryBytes {
		return invalid("role_entry_limit", "role entry exceeds its size limit")
	}
	candidate, parseErr := ParseMarkdown(raw, ParseOptions{Origin: origin, Limits: limits, Redactor: redactor})
	if parseErr != nil {
		return invalid("role_entry_failed", "role entry cannot be parsed")
	}
	return candidate, int64(len(raw))
}

func lstatRootWithoutSymlinks(fileSystem fs.ReadLinkFS, root string) (fs.FileInfo, error) {
	current := ""
	var info fs.FileInfo
	for _, component := range strings.Split(root, "/") {
		if current == "" {
			current = component
		} else {
			current = path.Join(current, component)
		}
		candidate, err := fileSystem.Lstat(current)
		if err != nil {
			return nil, err
		}
		if candidate.Mode()&fs.ModeSymlink != 0 {
			return candidate, nil
		}
		info = candidate
	}
	return info, nil
}

func safeDiscoveryError(redactor *redact.RuntimeRedactor, code, message string) error {
	safeMessage := redact.NewRuntimeRedactor().Redact(message)
	if redactor != nil {
		safeMessage = redactor.Redact(message)
	}
	return &diagnostics.SafeError{
		Code:        code,
		Source:      "agentrole",
		Message:     safeMessage,
		Recoverable: false,
	}
}

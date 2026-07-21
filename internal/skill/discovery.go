package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

type sourceEntry struct {
	source       Source
	fsys         fs.FS
	fsPath       string
	entryPath    string
	packageRoot  string
	physical     bool
	physicalRoot string
	size         int64
	mode         fs.FileMode
	modTime      time.Time
	changeToken  string
	rejected     string
}

func discover(source SourceFS, limits Limits, redactor func(string) string) ([]Definition, []diagnostics.Diagnostic, error) {
	return discoverContext(context.Background(), source, limits, redactor)
}

func discoverContext(ctx context.Context, source SourceFS, limits Limits, redactor func(string) string) ([]Definition, []diagnostics.Diagnostic, error) {
	limits = normalizeLimits(limits)
	entries, listDiagnostics, err := listSourceEntries(ctx, source, limits, redactor)
	if err != nil {
		return nil, nil, err
	}
	definitions := make([]Definition, 0, len(entries))
	diagnosticItems := append([]diagnostics.Diagnostic(nil), listDiagnostics...)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if entry.rejected != "" {
			diagnosticItems = append(diagnosticItems, discoveryDiagnostic("skill_entry_rejected", entry.rejected, entry, redactor))
			continue
		}
		if entry.size > limits.MaxEntryBytes {
			diagnosticItems = append(diagnosticItems, discoveryDiagnostic("skill_entry_too_large", fmt.Sprintf("skill entry exceeds %d bytes", limits.MaxEntryBytes), entry, redactor))
			continue
		}
		data, err := readSourceEntry(entry, limits.MaxEntryBytes)
		if err != nil {
			diagnosticItems = append(diagnosticItems, discoveryDiagnostic("skill_entry_read_failed", "unable to read skill entry", entry, redactor))
			continue
		}
		metadata, body, err := ParseWithLimits(data, limits)
		if err != nil {
			diagnosticItems = append(diagnosticItems, discoveryDiagnostic("skill_entry_invalid", err.Error(), entry, redactor))
			continue
		}
		sum := sha256.Sum256(data)
		definitions = append(definitions, Definition{
			Metadata:    metadata,
			Body:        body,
			EntryPath:   entry.entryPath,
			PackageRoot: entry.packageRoot,
			Source:      source.Source,
			Fingerprint: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(definitions, func(i, j int) bool {
		if definitions[i].Metadata.Name != definitions[j].Metadata.Name {
			return definitions[i].Metadata.Name < definitions[j].Metadata.Name
		}
		return definitions[i].EntryPath < definitions[j].EntryPath
	})
	sortDiagnostics(diagnosticItems)
	return definitions, diagnosticItems, nil
}

func listSourceEntries(ctx context.Context, source SourceFS, limits Limits, redactor func(string) string) ([]sourceEntry, []diagnostics.Diagnostic, error) {
	if !validSource(source.Source) {
		return nil, nil, fmt.Errorf("unknown skill source")
	}
	if source.FS == nil {
		return listPhysicalEntries(ctx, source, limits, redactor)
	}
	return listFSEntries(ctx, source, limits, redactor)
}

func listPhysicalEntries(ctx context.Context, source SourceFS, limits Limits, redactor func(string) string) ([]sourceEntry, []diagnostics.Diagnostic, error) {
	root := strings.TrimSpace(source.Root)
	if root == "" {
		root = "."
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, fmt.Errorf("normalize skill source root")
	}
	info, err := os.Lstat(absRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		diagnostic := diagnostics.New("skill_source_unreadable", diagnostics.SeverityWarning, "unable to inspect skill source").WithPath(safeValue(absRoot, redactor)).WithSource(string(source.Source))
		return nil, []diagnostics.Diagnostic{diagnostic}, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("skill source root cannot be a symbolic link")
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("skill source root is not a directory")
	}
	canonicalRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve skill source root")
	}
	dirEntries, err := os.ReadDir(canonicalRoot)
	if err != nil {
		diagnostic := diagnostics.New("skill_source_unreadable", diagnostics.SeverityWarning, "unable to read skill source").WithPath(safeValue(canonicalRoot, redactor)).WithSource(string(source.Source))
		return nil, []diagnostics.Diagnostic{diagnostic}, nil
	}
	entries := make([]sourceEntry, 0)
	for _, item := range dirEntries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		itemPath := filepath.Join(canonicalRoot, item.Name())
		if item.Type()&os.ModeSymlink != 0 {
			if strings.HasSuffix(item.Name(), ".md") {
				entries = append(entries, sourceEntry{source: source.Source, entryPath: itemPath, packageRoot: canonicalRoot, physical: true, physicalRoot: canonicalRoot, rejected: "symbolic link skill entries are not allowed"})
			}
			continue
		}
		if item.IsDir() {
			entryPath := filepath.Join(itemPath, "SKILL.md")
			entryInfo, statErr := os.Lstat(entryPath)
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
			entry := sourceEntry{source: source.Source, fsPath: entryPath, entryPath: entryPath, packageRoot: itemPath, physical: true, physicalRoot: canonicalRoot}
			if statErr != nil {
				entry.rejected = "unable to inspect skill entry"
			} else {
				fillEntryInfo(&entry, entryInfo)
				if entryInfo.Mode()&os.ModeSymlink != 0 {
					entry.rejected = "symbolic link skill entries are not allowed"
				} else if !entryInfo.Mode().IsRegular() {
					entry.rejected = "skill entry is not a regular file"
				}
			}
			entries = append(entries, entry)
			continue
		}
		if path.Ext(item.Name()) != ".md" {
			continue
		}
		entryInfo, statErr := os.Lstat(itemPath)
		entry := sourceEntry{source: source.Source, fsPath: itemPath, entryPath: itemPath, packageRoot: canonicalRoot, physical: true, physicalRoot: canonicalRoot}
		if statErr != nil {
			entry.rejected = "unable to inspect skill entry"
		} else {
			fillEntryInfo(&entry, entryInfo)
			if !entryInfo.Mode().IsRegular() {
				entry.rejected = "skill entry is not a regular file"
			}
		}
		entries = append(entries, entry)
	}
	return limitAndSortEntries(entries, source, limits, redactor), overflowDiagnostics(entries, source, limits, redactor), nil
}

func listFSEntries(ctx context.Context, source SourceFS, limits Limits, redactor func(string) string) ([]sourceEntry, []diagnostics.Diagnostic, error) {
	fsRoot, displayRoot, err := normalizeFSRoot(source.Root)
	if err != nil {
		return nil, nil, err
	}
	dirEntries, err := fs.ReadDir(source.FS, fsRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		diagnostic := diagnostics.New("skill_source_unreadable", diagnostics.SeverityWarning, "unable to read skill source").WithPath(safeValue(displayRoot, redactor)).WithSource(string(source.Source))
		return nil, []diagnostics.Diagnostic{diagnostic}, nil
	}
	entries := make([]sourceEntry, 0)
	for _, item := range dirEntries {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		itemFSPath := path.Join(fsRoot, item.Name())
		itemDisplayPath := joinDisplayPath(displayRoot, item.Name())
		if item.Type()&fs.ModeSymlink != 0 {
			if strings.HasSuffix(item.Name(), ".md") {
				entries = append(entries, sourceEntry{source: source.Source, fsys: source.FS, fsPath: itemFSPath, entryPath: itemDisplayPath, packageRoot: displayRoot, rejected: "symbolic link skill entries are not allowed"})
			}
			continue
		}
		if item.IsDir() {
			entryFSPath := path.Join(itemFSPath, "SKILL.md")
			entryDisplayPath := joinDisplayPath(itemDisplayPath, "SKILL.md")
			children, readErr := fs.ReadDir(source.FS, itemFSPath)
			if readErr != nil {
				entries = append(entries, sourceEntry{source: source.Source, fsys: source.FS, fsPath: entryFSPath, entryPath: entryDisplayPath, packageRoot: itemDisplayPath, rejected: "unable to inspect skill package"})
				continue
			}
			var entryItem fs.DirEntry
			for _, child := range children {
				if child.Name() == "SKILL.md" {
					entryItem = child
					break
				}
			}
			if entryItem == nil {
				continue
			}
			packageRoot := itemDisplayPath
			if source.Source == SourceBuiltin {
				packageRoot = "builtin://" + item.Name()
			}
			entry := sourceEntry{source: source.Source, fsys: source.FS, fsPath: entryFSPath, entryPath: entryDisplayPath, packageRoot: packageRoot}
			entryInfo, statErr := entryItem.Info()
			if entryItem.Type()&fs.ModeSymlink != 0 {
				entry.rejected = "symbolic link skill entries are not allowed"
			} else if statErr != nil {
				entry.rejected = "unable to inspect skill entry"
			} else {
				fillEntryInfo(&entry, entryInfo)
				if !entryInfo.Mode().IsRegular() {
					entry.rejected = "skill entry is not a regular file"
				}
			}
			entries = append(entries, entry)
			continue
		}
		if path.Ext(item.Name()) != ".md" {
			continue
		}
		entryInfo, statErr := item.Info()
		entry := sourceEntry{source: source.Source, fsys: source.FS, fsPath: itemFSPath, entryPath: itemDisplayPath, packageRoot: displayRoot}
		if statErr != nil {
			entry.rejected = "unable to inspect skill entry"
		} else {
			fillEntryInfo(&entry, entryInfo)
			if !entryInfo.Mode().IsRegular() {
				entry.rejected = "skill entry is not a regular file"
			}
		}
		entries = append(entries, entry)
	}
	return limitAndSortEntries(entries, source, limits, redactor), overflowDiagnostics(entries, source, limits, redactor), nil
}

func limitAndSortEntries(entries []sourceEntry, _ SourceFS, limits Limits, _ func(string) string) []sourceEntry {
	sort.Slice(entries, func(i, j int) bool { return entries[i].entryPath < entries[j].entryPath })
	if len(entries) > limits.MaxFiles {
		return entries[:limits.MaxFiles]
	}
	return entries
}

func overflowDiagnostics(entries []sourceEntry, source SourceFS, limits Limits, redactor func(string) string) []diagnostics.Diagnostic {
	if len(entries) <= limits.MaxFiles {
		return nil
	}
	message := fmt.Sprintf("skill source contains %d entries, exceeding the limit of %d; excess entries were skipped", len(entries), limits.MaxFiles)
	return []diagnostics.Diagnostic{diagnostics.New("skill_source_entry_limit", diagnostics.SeverityWarning, message).WithPath(safeValue(source.Root, redactor)).WithSource(string(source.Source))}
}

func readSourceEntry(entry sourceEntry, maxBytes int64) ([]byte, error) {
	var reader io.ReadCloser
	var err error
	if entry.physical {
		reader, err = openPhysicalEntryNoFollow(entry.physicalRoot, entry.fsPath)
	} else {
		reader, err = entry.fsys.Open(entry.fsPath)
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("entry exceeds size limit")
	}
	return data, nil
}

func openPhysicalEntryNoFollow(root string, filename string) (*os.File, error) {
	root = filepath.Clean(root)
	filename = filepath.Clean(filename)
	relative, err := filepath.Rel(root, filename)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return nil, fmt.Errorf("skill entry is outside its source root")
	}
	parts := strings.Split(relative, string(os.PathSeparator))
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	currentFD := rootFD
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			unix.Close(currentFD)
			return nil, fmt.Errorf("invalid skill entry path")
		}
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if index < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}
		nextFD, openErr := unix.Openat(currentFD, part, flags, 0)
		unix.Close(currentFD)
		if openErr != nil {
			return nil, openErr
		}
		currentFD = nextFD
	}
	file := os.NewFile(uintptr(currentFD), filename)
	if file == nil {
		unix.Close(currentFD)
		return nil, fmt.Errorf("open skill entry")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("skill entry is not a regular file")
	}
	return file, nil
}

func fingerprintSources(ctx context.Context, sources []SourceFS, limits Limits) (string, error) {
	limits = normalizeLimits(limits)
	type manifestSource struct {
		source SourceFS
		key    string
	}
	ordered := make([]manifestSource, 0, len(sources))
	for _, source := range sources {
		ordered = append(ordered, manifestSource{source: source, key: strconv.Itoa(sourceRank(source.Source)) + "\x00" + string(source.Source) + "\x00" + source.Root})
	}
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].key < ordered[j].key })
	hash := sha256.New()
	for _, item := range ordered {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		entries, diagnosticItems, err := listSourceEntries(ctx, item.source, limits, nil)
		if err != nil {
			return "", err
		}
		io.WriteString(hash, item.key)
		io.WriteString(hash, "\n")
		for _, entry := range entries {
			io.WriteString(hash, entry.entryPath)
			io.WriteString(hash, "\x00")
			io.WriteString(hash, strconv.FormatInt(entry.size, 10))
			io.WriteString(hash, "\x00")
			io.WriteString(hash, strconv.FormatInt(entry.modTime.UnixNano(), 10))
			io.WriteString(hash, "\x00")
			io.WriteString(hash, entry.mode.String())
			io.WriteString(hash, "\x00")
			io.WriteString(hash, entry.changeToken)
			io.WriteString(hash, "\x00")
			io.WriteString(hash, entry.rejected)
			io.WriteString(hash, "\n")
		}
		for _, diagnostic := range diagnosticItems {
			io.WriteString(hash, diagnostic.Code)
			io.WriteString(hash, "\x00")
			io.WriteString(hash, diagnostic.Message)
			io.WriteString(hash, "\x00")
			io.WriteString(hash, diagnostic.Source)
			io.WriteString(hash, "\x00")
			io.WriteString(hash, diagnostic.Path)
			io.WriteString(hash, "\n")
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fillEntryInfo(entry *sourceEntry, info fs.FileInfo) {
	entry.size = info.Size()
	entry.mode = info.Mode()
	entry.modTime = info.ModTime()
	entry.changeToken = fileChangeToken(info)
}

func fileChangeToken(info fs.FileInfo) string {
	if info == nil || info.Sys() == nil {
		return ""
	}
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return ""
	}
	parts := make([]string, 0, 3)
	for _, name := range []string{"Ino", "Ctim", "Ctimespec", "Ctime", "Ctimensec", "FileIndexHigh", "FileIndexLow"} {
		field := value.FieldByName(name)
		if field.IsValid() && field.CanInterface() {
			parts = append(parts, name+"="+fmt.Sprint(field.Interface()))
		}
	}
	return strings.Join(parts, ";")
}

func discoveryDiagnostic(code, message string, entry sourceEntry, redactor func(string) string) diagnostics.Diagnostic {
	return diagnostics.New(code, diagnostics.SeverityWarning, safeValue(message, redactor)).WithPath(safeValue(entry.entryPath, redactor)).WithSource(string(entry.source))
}

func normalizeFSRoot(root string) (string, string, error) {
	displayRoot := strings.TrimSpace(root)
	if displayRoot == "" {
		displayRoot = "."
	}
	slashed := filepath.ToSlash(displayRoot)
	fsRoot := strings.TrimPrefix(slashed, "/")
	fsRoot = path.Clean(fsRoot)
	if fsRoot == "" {
		fsRoot = "."
	}
	if !fs.ValidPath(fsRoot) {
		return "", "", fmt.Errorf("invalid skill source root")
	}
	return fsRoot, filepath.Clean(displayRoot), nil
}

func joinDisplayPath(root, name string) string {
	if root == "." {
		return filepath.Clean(name)
	}
	return filepath.Join(root, filepath.FromSlash(name))
}

func validSource(source Source) bool {
	switch source {
	case SourceBuiltin, SourceUser, SourceProject:
		return true
	default:
		return false
	}
}

func sourceRank(source Source) int {
	switch source {
	case SourceBuiltin:
		return 0
	case SourceUser:
		return 1
	case SourceProject:
		return 2
	default:
		return -1
	}
}

func safeValue(value string, redactor func(string) string) string {
	if redactor != nil {
		return redactor(value)
	}
	return redact.Text(value)
}

func sortDiagnostics(items []diagnostics.Diagnostic) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Code != items[j].Code {
			return items[i].Code < items[j].Code
		}
		if items[i].Source != items[j].Source {
			return items[i].Source < items[j].Source
		}
		if items[i].Path != items[j].Path {
			return items[i].Path < items[j].Path
		}
		return items[i].Message < items[j].Message
	})
}

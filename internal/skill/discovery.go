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

	"xagent/internal/budget"
	"xagent/internal/diagnostics"
	"xagent/internal/redact"
	"xagent/internal/safefs"
)

type sourceEntry struct {
	source       Source
	fsys         fs.FS
	fsPath       string
	entryPath    string
	packageRoot  string
	physicalRoot *safefs.Root
	size         int64
	mode         fs.FileMode
	modTime      time.Time
	changeToken  string
	rejected     string
}

type sourceListing struct {
	entries     []sourceEntry
	diagnostics []diagnostics.Diagnostic
	root        *safefs.Root
}

func (l *sourceListing) close() error {
	if l == nil || l.root == nil {
		return nil
	}
	return l.root.Close()
}

func discover(source SourceFS, limits Limits, redactor func(string) string) ([]Definition, []diagnostics.Diagnostic, error) {
	return discoverContext(context.Background(), source, limits, redactor)
}

func discoverContext(ctx context.Context, source SourceFS, limits Limits, redactor func(string) string) ([]Definition, []diagnostics.Diagnostic, error) {
	limits = normalizeLimits(limits)
	listing, err := listSourceEntries(ctx, source, limits, redactor)
	if err != nil {
		return nil, nil, err
	}
	defer listing.close()
	definitions := make([]Definition, 0, len(listing.entries))
	diagnosticItems := append([]diagnostics.Diagnostic(nil), listing.diagnostics...)
	for _, entry := range listing.entries {
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
		data, err := readSourceEntry(ctx, entry, limits.MaxEntryBytes)
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
	if err := listing.close(); err != nil {
		return nil, nil, fmt.Errorf("close skill source root")
	}
	return definitions, diagnosticItems, nil
}

func listSourceEntries(ctx context.Context, source SourceFS, limits Limits, redactor func(string) string) (*sourceListing, error) {
	if !validSource(source.Source) {
		return nil, fmt.Errorf("unknown skill source")
	}
	if source.FS == nil {
		return listPhysicalEntries(ctx, source, limits, redactor)
	}
	return listFSEntries(ctx, source, limits, redactor)
}

func listPhysicalEntries(ctx context.Context, source SourceFS, limits Limits, redactor func(string) string) (*sourceListing, error) {
	root := strings.TrimSpace(source.Root)
	if root == "" {
		root = "."
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("normalize skill source root")
	}
	absRoot = filepath.Clean(absRoot)
	info, err := os.Lstat(absRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return &sourceListing{}, nil
	}
	if err != nil {
		diagnostic := diagnostics.New("skill_source_unreadable", diagnostics.SeverityWarning, "unable to inspect skill source").WithPath(safeValue(absRoot, redactor)).WithSource(string(source.Source))
		return &sourceListing{diagnostics: []diagnostics.Diagnostic{diagnostic}}, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("skill source root cannot be a symbolic link")
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("skill source root is not a directory")
	}
	opened, err := safefs.Bootstrap(absRoot, safefs.Policy{})
	if err != nil || opened.Root == nil {
		diagnostic := diagnostics.New("skill_source_unreadable", diagnostics.SeverityWarning, "unable to safely open skill source").WithPath(safeValue(absRoot, redactor)).WithSource(string(source.Source))
		return &sourceListing{diagnostics: []diagnostics.Diagnostic{diagnostic}}, nil
	}
	displayRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		_ = opened.Root.Close()
		return nil, fmt.Errorf("resolve skill source display root")
	}
	listing := &sourceListing{root: opened.Root}
	counter, err := newSkillWalkCounter(limits)
	if err != nil {
		_ = listing.close()
		return nil, err
	}
	err = opened.Root.Walk(ctx, ".", counter, func(observed safefs.Entry) error {
		entry, include := physicalSkillEntry(source.Source, displayRoot, opened.Root, observed)
		if include {
			listing.entries = append(listing.entries, entry)
		}
		return nil
	})
	if err != nil {
		listing.entries = nil
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = listing.close()
			return nil, ctxErr
		}
		var limitErr *budget.LimitError
		if errors.As(err, &limitErr) {
			message := "skill source traversal exceeded its bounded file or directory limit; source was skipped"
			listing.diagnostics = append(listing.diagnostics, diagnostics.New("skill_source_scan_limit", diagnostics.SeverityWarning, message).WithPath(safeValue(absRoot, redactor)).WithSource(string(source.Source)))
			return listing, nil
		}
		listing.diagnostics = append(listing.diagnostics, diagnostics.New("skill_source_unreadable", diagnostics.SeverityWarning, "unable to safely traverse skill source").WithPath(safeValue(absRoot, redactor)).WithSource(string(source.Source)))
		return listing, nil
	}
	listing.diagnostics = overflowDiagnostics(listing.entries, source, limits, redactor)
	listing.entries = limitAndSortEntries(listing.entries, source, limits, redactor)
	return listing, nil
}

func listFSEntries(ctx context.Context, source SourceFS, limits Limits, redactor func(string) string) (*sourceListing, error) {
	fsRoot, displayRoot, err := normalizeFSRoot(source.Root)
	if err != nil {
		return nil, err
	}
	dirEntries, err := fs.ReadDir(source.FS, fsRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return &sourceListing{}, nil
	}
	if err != nil {
		diagnostic := diagnostics.New("skill_source_unreadable", diagnostics.SeverityWarning, "unable to read skill source").WithPath(safeValue(displayRoot, redactor)).WithSource(string(source.Source))
		return &sourceListing{diagnostics: []diagnostics.Diagnostic{diagnostic}}, nil
	}
	entries := make([]sourceEntry, 0)
	for _, item := range dirEntries {
		if err := ctx.Err(); err != nil {
			return nil, err
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
	return &sourceListing{
		entries:     limitAndSortEntries(entries, source, limits, redactor),
		diagnostics: overflowDiagnostics(entries, source, limits, redactor),
	}, nil
}

func newSkillWalkCounter(limits Limits) (*budget.Counter, error) {
	maxObserved := limits.MaxFiles
	if maxObserved < DefaultMaxFiles {
		maxObserved = DefaultMaxFiles
	}
	scanLimit := int64(maxObserved) + 1
	if scanLimit <= 1 {
		return nil, fmt.Errorf("skill traversal limit is invalid")
	}
	bounded, err := budget.NewLimits(
		budget.Limit{Dimension: budget.Files, Value: scanLimit},
		budget.Limit{Dimension: budget.Directories, Value: scanLimit},
	)
	if err != nil {
		return nil, err
	}
	return budget.NewCounter(bounded, bounded)
}

func physicalSkillEntry(source Source, rootPath string, root *safefs.Root, observed safefs.Entry) (sourceEntry, bool) {
	components := strings.Split(observed.Path, "/")
	if len(components) == 0 || len(components) > 2 {
		return sourceEntry{}, false
	}
	packageRelative := "."
	if len(components) == 1 {
		if observed.IsDir() || path.Ext(observed.Name) != ".md" {
			return sourceEntry{}, false
		}
	} else {
		if observed.Name != "SKILL.md" {
			return sourceEntry{}, false
		}
		packageRelative = components[0]
	}
	entry := sourceEntry{
		source:       source,
		fsPath:       observed.Path,
		entryPath:    filepath.Join(rootPath, filepath.FromSlash(observed.Path)),
		packageRoot:  rootPath,
		physicalRoot: root,
		size:         observed.Size,
		mode:         observed.Mode,
	}
	if packageRelative != "." {
		entry.packageRoot = filepath.Join(rootPath, filepath.FromSlash(packageRelative))
	}
	if observed.Mode&fs.ModeSymlink != 0 {
		entry.rejected = "symbolic link skill entries are not allowed"
		return entry, true
	}
	if !observed.Mode.IsRegular() {
		entry.rejected = "skill entry is not a regular file"
		return entry, true
	}
	binding, err := root.Bind(observed.Path)
	if err != nil {
		entry.rejected = "unable to inspect skill entry"
		return entry, true
	}
	encoded, err := binding.MarshalBinary()
	if err != nil {
		entry.rejected = "unable to inspect skill entry"
		return entry, true
	}
	entry.changeToken = hex.EncodeToString(encoded)
	return entry, true
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

func readSourceEntry(ctx context.Context, entry sourceEntry, maxBytes int64) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("skill entry context is invalid")
	}
	var reader io.ReadCloser
	var err error
	if entry.physicalRoot != nil {
		if err := verifyPhysicalEntryBinding(entry); err != nil {
			return nil, err
		}
		reader, err = entry.physicalRoot.OpenRead(ctx, entry.fsPath)
	} else {
		reader, err = entry.fsys.Open(entry.fsPath)
	}
	if err != nil {
		return nil, err
	}
	data, readErr := readBoundedSkillEntry(ctx, reader, maxBytes)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close skill entry")
	}
	if entry.physicalRoot != nil {
		if err := verifyPhysicalEntryBinding(entry); err != nil {
			return nil, err
		}
	}
	return data, nil
}

func readBoundedSkillEntry(ctx context.Context, reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("skill entry byte limit is invalid")
	}
	data := make([]byte, 0, min(maxBytes, 32*1024))
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := maxBytes - int64(len(data))
		readSize := int64(len(buffer))
		if remaining < readSize {
			readSize = remaining + 1
		}
		count, readErr := reader.Read(buffer[:int(readSize)])
		if count > 0 {
			if int64(count) > remaining {
				return nil, fmt.Errorf("entry exceeds size limit")
			}
			data = append(data, buffer[:count]...)
		}
		if errors.Is(readErr, io.EOF) {
			return data, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		if count == 0 {
			return nil, fmt.Errorf("skill entry read made no progress")
		}
	}
}

func verifyPhysicalEntryBinding(entry sourceEntry) error {
	if entry.physicalRoot == nil || entry.changeToken == "" {
		return fmt.Errorf("skill entry binding is unavailable")
	}
	binding, err := entry.physicalRoot.Bind(entry.fsPath)
	if err != nil {
		return fmt.Errorf("skill entry binding changed")
	}
	encoded, err := binding.MarshalBinary()
	if err != nil || hex.EncodeToString(encoded) != entry.changeToken {
		return fmt.Errorf("skill entry binding changed")
	}
	return nil
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
		listing, err := listSourceEntries(ctx, item.source, limits, nil)
		if err != nil {
			return "", err
		}
		io.WriteString(hash, item.key)
		io.WriteString(hash, "\n")
		for _, entry := range listing.entries {
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
			if entry.physicalRoot != nil && entry.rejected == "" && entry.size <= limits.MaxEntryBytes {
				data, readErr := readSourceEntry(ctx, entry, limits.MaxEntryBytes)
				if readErr != nil {
					io.WriteString(hash, "\x00read-error")
				} else {
					digest := sha256.Sum256(data)
					io.WriteString(hash, "\x00")
					io.WriteString(hash, hex.EncodeToString(digest[:]))
				}
			}
			io.WriteString(hash, "\n")
		}
		for _, diagnostic := range listing.diagnostics {
			io.WriteString(hash, diagnostic.Code)
			io.WriteString(hash, "\x00")
			io.WriteString(hash, diagnostic.Message)
			io.WriteString(hash, "\x00")
			io.WriteString(hash, diagnostic.Source)
			io.WriteString(hash, "\x00")
			io.WriteString(hash, diagnostic.Path)
			io.WriteString(hash, "\n")
		}
		if err := listing.close(); err != nil {
			return "", fmt.Errorf("close skill source root")
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

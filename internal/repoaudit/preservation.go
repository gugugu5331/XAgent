package repoaudit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

const PreservationVersion = 1

// PreservationManifest binds exact index entries to no-follow worktree
// fingerprints. Persistence belongs to the evidence owner, not this model.
type PreservationManifest struct {
	Version          int
	PreIndexSHA256   string
	GitmodulesAbsent bool
	Entries          []PreservationEntry
}

type PreservationEntry struct {
	Path        []byte
	Mode        uint32
	OID         string
	Stage       int
	Fingerprint WorktreeFingerprint
}

type WorktreeFingerprint struct {
	Kind      string
	Mode      uint32
	Size      int64
	SHA256    string
	Inventory []InventoryEntry
}

type InventoryEntry struct {
	Path   []byte
	Kind   string
	Mode   uint32
	Size   int64
	SHA256 string
}

func BuildPreservationManifest(worktree string, selected []Entry) (PreservationManifest, error) {
	root, err := os.OpenRoot(worktree)
	if err != nil {
		return PreservationManifest{}, fmt.Errorf("open preservation root: %w", err)
	}
	defer root.Close()

	entries := append([]Entry(nil), selected...)
	sort.Slice(entries, func(i, j int) bool { return bytes.Compare([]byte(entries[i].Path), []byte(entries[j].Path)) < 0 })
	manifest := PreservationManifest{Version: PreservationVersion, Entries: make([]PreservationEntry, 0, len(entries))}
	for _, entry := range entries {
		if entry.Stage != 0 || !isLowerGitOID(entry.OID) {
			return PreservationManifest{}, errors.New("preservation selection requires canonical stage-0 index entries")
		}
		fingerprint, err := fingerprintAt(root, filepath.ToSlash(entry.Path))
		if err != nil {
			return PreservationManifest{}, fmt.Errorf("fingerprint preservation target: %w", err)
		}
		manifest.Entries = append(manifest.Entries, PreservationEntry{
			Path:        append([]byte(nil), []byte(entry.Path)...),
			Mode:        entry.Mode,
			OID:         entry.OID,
			Stage:       entry.Stage,
			Fingerprint: fingerprint,
		})
	}
	return manifest, nil
}

func VerifyPreservationManifest(worktree string, manifest PreservationManifest, selected []Entry) error {
	if manifest.Version != PreservationVersion {
		return errors.New("unsupported preservation manifest version")
	}
	current, err := BuildPreservationManifest(worktree, selected)
	if err != nil {
		return err
	}
	current.PreIndexSHA256 = manifest.PreIndexSHA256
	current.GitmodulesAbsent = manifest.GitmodulesAbsent
	want, err := CanonicalPreservationBytes(manifest)
	if err != nil {
		return err
	}
	got, err := CanonicalPreservationBytes(current)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("preservation manifest identity or content changed")
	}
	return nil
}

// CanonicalPreservationBytes produces a NUL-safe deterministic encoding.
func CanonicalPreservationBytes(manifest PreservationManifest) ([]byte, error) {
	if manifest.Version != PreservationVersion {
		return nil, errors.New("unsupported preservation manifest version")
	}
	var output bytes.Buffer
	output.WriteString("xagent-preservation-v1\x00")
	if !isLowerSHA256(manifest.PreIndexSHA256) {
		return nil, errors.New("preservation pre-index digest is invalid")
	}
	writeBytesField(&output, []byte(manifest.PreIndexSHA256))
	if manifest.GitmodulesAbsent {
		writeNumberField(&output, 1)
	} else {
		writeNumberField(&output, 0)
	}
	writeNumberField(&output, int64(len(manifest.Entries)))
	for _, entry := range manifest.Entries {
		if bytes.IndexByte(entry.Path, 0) >= 0 {
			return nil, errors.New("preservation path contains NUL")
		}
		writeBytesField(&output, entry.Path)
		writeNumberField(&output, int64(entry.Mode))
		writeBytesField(&output, []byte(entry.OID))
		writeNumberField(&output, int64(entry.Stage))
		writeFingerprint(&output, entry.Fingerprint)
	}
	return output.Bytes(), nil
}

// ParsePreservationBytes strictly decodes the canonical NUL-safe format.
func ParsePreservationBytes(data []byte) (PreservationManifest, error) {
	const prefix = "xagent-preservation-v1\x00"
	if !bytes.HasPrefix(data, []byte(prefix)) {
		return PreservationManifest{}, errors.New("preservation manifest prefix is invalid")
	}
	decoder := preservationDecoder{data: data, offset: len(prefix)}
	preIndex, err := decoder.bytesField()
	if err != nil || !isLowerSHA256(string(preIndex)) {
		return PreservationManifest{}, errors.New("preservation pre-index digest is invalid")
	}
	absent, err := decoder.numberField()
	if err != nil || absent < 0 || absent > 1 {
		return PreservationManifest{}, errors.New("preservation .gitmodules sentinel is invalid")
	}
	count, err := decoder.numberField()
	if err != nil || count < 0 || count > 1000 {
		return PreservationManifest{}, errors.New("preservation entry count is invalid")
	}
	manifest := PreservationManifest{
		Version:          PreservationVersion,
		PreIndexSHA256:   string(preIndex),
		GitmodulesAbsent: absent == 1,
		Entries:          make([]PreservationEntry, 0, count),
	}
	for index := int64(0); index < count; index++ {
		entry, err := decoder.entry()
		if err != nil {
			return PreservationManifest{}, err
		}
		manifest.Entries = append(manifest.Entries, entry)
	}
	if decoder.offset != len(decoder.data) {
		return PreservationManifest{}, errors.New("preservation manifest has trailing bytes")
	}
	canonical, err := CanonicalPreservationBytes(manifest)
	if err != nil || !bytes.Equal(canonical, data) {
		return PreservationManifest{}, errors.New("preservation manifest is not canonical")
	}
	return manifest, nil
}

type preservationDecoder struct {
	data   []byte
	offset int
}

func (d *preservationDecoder) entry() (PreservationEntry, error) {
	name, err := d.bytesField()
	if err != nil || len(name) == 0 || bytes.IndexByte(name, 0) >= 0 {
		return PreservationEntry{}, errors.New("preservation entry path is invalid")
	}
	mode, err := d.numberField()
	if err != nil || mode < 0 || mode > int64(^uint32(0)) {
		return PreservationEntry{}, errors.New("preservation entry mode is invalid")
	}
	oid, err := d.bytesField()
	if err != nil || !isLowerGitOID(string(oid)) {
		return PreservationEntry{}, errors.New("preservation entry object ID is invalid")
	}
	stage, err := d.numberField()
	if err != nil || stage != 0 {
		return PreservationEntry{}, errors.New("preservation entry stage is invalid")
	}
	fingerprint, err := d.fingerprint()
	if err != nil {
		return PreservationEntry{}, err
	}
	return PreservationEntry{Path: append([]byte(nil), name...), Mode: uint32(mode), OID: string(oid), Stage: 0, Fingerprint: fingerprint}, nil
}

func (d *preservationDecoder) fingerprint() (WorktreeFingerprint, error) {
	kind, err := d.bytesField()
	if err != nil || string(kind) != "regular" && string(kind) != "symlink" && string(kind) != "directory" {
		return WorktreeFingerprint{}, errors.New("preservation fingerprint kind is invalid")
	}
	mode, err := d.numberField()
	if err != nil || mode < 0 || mode > int64(^uint32(0)) {
		return WorktreeFingerprint{}, errors.New("preservation fingerprint mode is invalid")
	}
	size, err := d.numberField()
	if err != nil || size < 0 {
		return WorktreeFingerprint{}, errors.New("preservation fingerprint size is invalid")
	}
	digest, err := d.bytesField()
	if err != nil || !isLowerSHA256(string(digest)) {
		return WorktreeFingerprint{}, errors.New("preservation fingerprint digest is invalid")
	}
	count, err := d.numberField()
	if err != nil || count < 0 || count > 1000000 {
		return WorktreeFingerprint{}, errors.New("preservation inventory count is invalid")
	}
	fingerprint := WorktreeFingerprint{Kind: string(kind), Mode: uint32(mode), Size: size, SHA256: string(digest), Inventory: make([]InventoryEntry, 0, count)}
	for index := int64(0); index < count; index++ {
		item, err := d.inventoryEntry()
		if err != nil {
			return WorktreeFingerprint{}, err
		}
		fingerprint.Inventory = append(fingerprint.Inventory, item)
	}
	if fingerprint.Kind != "directory" && len(fingerprint.Inventory) != 0 {
		return WorktreeFingerprint{}, errors.New("non-directory preservation fingerprint has inventory")
	}
	return fingerprint, nil
}

func (d *preservationDecoder) inventoryEntry() (InventoryEntry, error) {
	name, err := d.bytesField()
	if err != nil || len(name) == 0 || bytes.IndexByte(name, 0) >= 0 {
		return InventoryEntry{}, errors.New("preservation inventory path is invalid")
	}
	kind, err := d.bytesField()
	if err != nil || string(kind) != "regular" && string(kind) != "symlink" && string(kind) != "directory" {
		return InventoryEntry{}, errors.New("preservation inventory kind is invalid")
	}
	mode, err := d.numberField()
	if err != nil || mode < 0 || mode > int64(^uint32(0)) {
		return InventoryEntry{}, errors.New("preservation inventory mode is invalid")
	}
	size, err := d.numberField()
	if err != nil || size < 0 {
		return InventoryEntry{}, errors.New("preservation inventory size is invalid")
	}
	digest, err := d.bytesField()
	if err != nil || !isLowerSHA256(string(digest)) {
		return InventoryEntry{}, errors.New("preservation inventory digest is invalid")
	}
	return InventoryEntry{Path: append([]byte(nil), name...), Kind: string(kind), Mode: uint32(mode), Size: size, SHA256: string(digest)}, nil
}

func (d *preservationDecoder) numberField() (int64, error) {
	field, err := d.bytesField()
	if err != nil || len(field) == 0 {
		return 0, errors.New("preservation number field is invalid")
	}
	if len(field) > 1 && field[0] == '0' || field[0] == '+' {
		return 0, errors.New("preservation number field is not canonical")
	}
	value, err := strconv.ParseInt(string(field), 10, 64)
	if err != nil {
		return 0, errors.New("preservation number field is invalid")
	}
	return value, nil
}

func (d *preservationDecoder) bytesField() ([]byte, error) {
	start := d.offset
	for d.offset < len(d.data) && d.data[d.offset] >= '0' && d.data[d.offset] <= '9' {
		d.offset++
	}
	if d.offset == start || d.offset >= len(d.data) || d.data[d.offset] != ':' {
		return nil, errors.New("preservation field length is invalid")
	}
	if d.offset-start > 1 && d.data[start] == '0' {
		return nil, errors.New("preservation field length is not canonical")
	}
	length, err := strconv.ParseUint(string(d.data[start:d.offset]), 10, 63)
	if err != nil {
		return nil, errors.New("preservation field length is invalid")
	}
	d.offset++
	if length > uint64(len(d.data)-d.offset) || int(length) == len(d.data)-d.offset {
		return nil, errors.New("preservation field is truncated")
	}
	end := d.offset + int(length)
	if d.data[end] != 0 {
		return nil, errors.New("preservation field terminator is invalid")
	}
	field := d.data[d.offset:end]
	d.offset = end + 1
	return field, nil
}

func PreservationSelectionDigest(entries []Entry) (string, error) {
	ordered := append([]Entry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return bytes.Compare([]byte(ordered[i].Path), []byte(ordered[j].Path)) < 0 })
	hash := sha256.New()
	for _, entry := range ordered {
		if entry.Stage != 0 || !isLowerGitOID(entry.OID) || bytes.IndexByte([]byte(entry.Path), 0) >= 0 {
			return "", errors.New("invalid preservation selection entry")
		}
		fmt.Fprintf(hash, "%06o %s %d\t", entry.Mode, entry.OID, entry.Stage)
		hash.Write([]byte(entry.Path))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fingerprintAt(root *os.Root, name string) (WorktreeFingerprint, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return WorktreeFingerprint{}, err
	}
	mode := info.Mode()
	switch {
	case mode.IsRegular():
		file, err := root.Open(name)
		if err != nil {
			return WorktreeFingerprint{}, err
		}
		digest, size, err := hashReader(file)
		closeErr := file.Close()
		if err != nil {
			return WorktreeFingerprint{}, err
		}
		if closeErr != nil {
			return WorktreeFingerprint{}, closeErr
		}
		return WorktreeFingerprint{Kind: "regular", Mode: uint32(mode), Size: size, SHA256: digest}, nil
	case mode&os.ModeSymlink != 0:
		target, err := root.Readlink(name)
		if err != nil {
			return WorktreeFingerprint{}, err
		}
		digest := sha256.Sum256([]byte(target))
		return WorktreeFingerprint{Kind: "symlink", Mode: uint32(mode), Size: int64(len(target)), SHA256: hex.EncodeToString(digest[:])}, nil
	case mode.IsDir():
		inventory, err := inventoryDirectory(root, name)
		if err != nil {
			return WorktreeFingerprint{}, err
		}
		var encoded bytes.Buffer
		for _, item := range inventory {
			writeInventoryEntry(&encoded, item)
		}
		digest := sha256.Sum256(encoded.Bytes())
		return WorktreeFingerprint{Kind: "directory", Mode: uint32(mode), Size: int64(len(inventory)), SHA256: hex.EncodeToString(digest[:]), Inventory: inventory}, nil
	default:
		return WorktreeFingerprint{}, errors.New("unsupported preservation target type")
	}
}

func inventoryDirectory(root *os.Root, name string) ([]InventoryEntry, error) {
	directory, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	names, err := directory.Readdirnames(-1)
	closeErr := directory.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	sort.Slice(names, func(i, j int) bool { return bytes.Compare([]byte(names[i]), []byte(names[j])) < 0 })
	var inventory []InventoryEntry
	for _, childName := range names {
		childPath := filepath.ToSlash(filepath.Join(name, childName))
		fingerprint, err := fingerprintAt(root, childPath)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(filepath.FromSlash(name), filepath.FromSlash(childPath))
		if err != nil {
			return nil, err
		}
		inventory = append(inventory, InventoryEntry{Path: []byte(filepath.ToSlash(relative)), Kind: fingerprint.Kind, Mode: fingerprint.Mode, Size: fingerprint.Size, SHA256: fingerprint.SHA256})
		if fingerprint.Kind == "directory" {
			for _, descendant := range fingerprint.Inventory {
				inventory = append(inventory, InventoryEntry{
					Path:   []byte(filepath.ToSlash(filepath.Join(relative, string(descendant.Path)))),
					Kind:   descendant.Kind,
					Mode:   descendant.Mode,
					Size:   descendant.Size,
					SHA256: descendant.SHA256,
				})
			}
		}
	}
	sort.Slice(inventory, func(i, j int) bool { return bytes.Compare(inventory[i].Path, inventory[j].Path) < 0 })
	return inventory, nil
}

func hashReader(reader io.Reader) (string, int64, error) {
	hash := sha256.New()
	size, err := io.Copy(hash, reader)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func writeFingerprint(output *bytes.Buffer, fingerprint WorktreeFingerprint) {
	writeBytesField(output, []byte(fingerprint.Kind))
	writeNumberField(output, int64(fingerprint.Mode))
	writeNumberField(output, fingerprint.Size)
	writeBytesField(output, []byte(fingerprint.SHA256))
	writeNumberField(output, int64(len(fingerprint.Inventory)))
	for _, item := range fingerprint.Inventory {
		writeInventoryEntry(output, item)
	}
}

func writeInventoryEntry(output *bytes.Buffer, item InventoryEntry) {
	writeBytesField(output, item.Path)
	writeBytesField(output, []byte(item.Kind))
	writeNumberField(output, int64(item.Mode))
	writeNumberField(output, item.Size)
	writeBytesField(output, []byte(item.SHA256))
}

func writeBytesField(output *bytes.Buffer, value []byte) {
	output.WriteString(strconv.Itoa(len(value)))
	output.WriteByte(':')
	output.Write(value)
	output.WriteByte(0)
}

func writeNumberField(output *bytes.Buffer, value int64) {
	writeBytesField(output, []byte(strconv.FormatInt(value, 10)))
}

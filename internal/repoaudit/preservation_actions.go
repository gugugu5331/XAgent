package repoaudit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
)

const (
	ApprovedPreservationCount  = 35
	ApprovedPreservationDigest = "0c44cba3839534b8ddd6d8153dc1def2f3f69ac0ea8acf745934ebd4064365f0"
)

func SelectPreservationEntries(entries []Entry) ([]Entry, error) {
	selected := make([]Entry, 0)
	for _, entry := range entries {
		if entry.Stage != 0 {
			return nil, errors.New("preservation index contains a non-zero stage")
		}
		if isPreservationTarget(entry.Path) {
			selected = append(selected, entry)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		return bytes.Compare([]byte(selected[i].Path), []byte(selected[j].Path)) < 0
	})
	return selected, nil
}

func ValidateApprovedPreservationSelection(entries []Entry) (string, error) {
	if len(entries) != ApprovedPreservationCount {
		return "", errors.New("preservation target count differs from the approved set")
	}
	digest, err := PreservationSelectionDigest(entries)
	if err != nil {
		return "", err
	}
	if digest != ApprovedPreservationDigest {
		return "", errors.New("preservation target digest differs from the approved set")
	}
	return digest, nil
}

func IndexIdentityDigest(entries []Entry) (string, error) {
	ordered := append([]Entry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool {
		return bytes.Compare([]byte(ordered[i].Path), []byte(ordered[j].Path)) < 0
	})
	hash := sha256.New()
	for index, entry := range ordered {
		if entry.Stage != 0 || !isLowerGitOID(entry.OID) || bytes.IndexByte([]byte(entry.Path), 0) >= 0 {
			return "", errors.New("invalid index identity entry")
		}
		if index > 0 && ordered[index-1].Path == entry.Path {
			return "", errors.New("duplicate index identity path")
		}
		writeBytesFieldToHash(hash, []byte(entry.Path))
		writeBytesFieldToHash(hash, []byte(entry.OID))
		writeBytesFieldToHash(hash, []byte(strings.ToLower(formatGitMode(entry.Mode))))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ManifestIndexEntries(manifest PreservationManifest) []Entry {
	entries := make([]Entry, 0, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		entries = append(entries, Entry{Path: string(entry.Path), Mode: entry.Mode, OID: entry.OID, Stage: entry.Stage, Type: gitTypeForMode(entry.Mode)})
	}
	return entries
}

func IndexWithoutPreservationTargets(entries []Entry, targets []Entry) []Entry {
	removed := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		removed[target.Path] = struct{}{}
	}
	result := make([]Entry, 0, len(entries)-len(targets))
	for _, entry := range entries {
		if _, ok := removed[entry.Path]; !ok {
			result = append(result, entry)
		}
	}
	return result
}

func isPreservationTarget(name string) bool {
	return strings.HasPrefix(name, ".claude/worktrees/") ||
		strings.HasPrefix(name, ".mewcode/memory/") ||
		strings.HasPrefix(name, ".mewcode/sessions/") ||
		name == "fakeprovider" || strings.HasSuffix(name, ".webarchive")
}

func formatGitMode(mode uint32) string {
	const digits = "01234567"
	buffer := [6]byte{'0', '0', '0', '0', '0', '0'}
	for index := len(buffer) - 1; index >= 0; index-- {
		buffer[index] = digits[mode&7]
		mode >>= 3
	}
	return string(buffer[:])
}

type hashWriter interface {
	Write([]byte) (int, error)
}

func writeBytesFieldToHash(hash hashWriter, value []byte) {
	hash.Write([]byte{byte(len(value) >> 24), byte(len(value) >> 16), byte(len(value) >> 8), byte(len(value))})
	hash.Write(value)
}

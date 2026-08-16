package worktree

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxMetadataBytes   = 1 << 20
	maxMetadataEntries = 10_000
	maxMetadataPath    = 4_096
)

type ManifestEntryType string

type ManifestEntryState string

const (
	ManifestFile      ManifestEntryType = "file"
	ManifestDirectory ManifestEntryType = "directory"
	ManifestSymlink   ManifestEntryType = "symlink"

	ManifestEntryPlanned ManifestEntryState = "planned"
	ManifestEntryCreated ManifestEntryState = "created"
)

type ManifestEntry struct {
	Path           string             `json:"path"`
	Type           ManifestEntryType  `json:"type"`
	State          ManifestEntryState `json:"state"`
	Size           int64              `json:"size,omitempty"`
	Digest         string             `json:"digest,omitempty"`
	Mode           os.FileMode        `json:"mode,omitempty"`
	IdentityDigest string             `json:"identity_digest,omitempty"`
}

type Manifest struct {
	SchemaVersion   int             `json:"schema_version"`
	WorkspaceID     string          `json:"workspace_id"`
	CreatedAt       time.Time       `json:"created_at"`
	Entries         []ManifestEntry `json:"entries"`
	IntegrityDigest string          `json:"integrity_digest"`
}

func (m Manifest) Clone() Manifest {
	clone := m
	clone.Entries = append([]ManifestEntry(nil), m.Entries...)
	return clone
}

func EncodeManifest(manifest Manifest) ([]byte, error) {
	if manifest.SchemaVersion == 0 {
		manifest.SchemaVersion = ManifestSchemaVersion
	}
	if err := validateManifest(manifest, false); err != nil {
		return nil, err
	}
	digest, err := manifestDigest(manifest)
	if err != nil {
		return nil, err
	}
	manifest.IntegrityDigest = digest
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", ErrInvalidMetadata)
	}
	if len(encoded) > maxMetadataBytes {
		return nil, ErrMetadataTooLarge
	}
	return append(encoded, '\n'), nil
}

func DecodeManifest(data []byte) (Manifest, error) {
	if len(data) > maxMetadataBytes {
		return Manifest{}, ErrMetadataTooLarge
	}
	var manifest Manifest
	if err := strictJSON(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", ErrInvalidMetadata)
	}
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return Manifest{}, ErrUnknownSchema
	}
	if err := validateManifest(manifest, true); err != nil {
		return Manifest{}, err
	}
	digest, err := manifestDigest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	if !equalDigest(manifest.IntegrityDigest, digest) {
		return Manifest{}, ErrIntegrityMismatch
	}
	return manifest, nil
}

func validateManifest(manifest Manifest, requireDigest bool) error {
	if manifest.SchemaVersion != ManifestSchemaVersion || !ValidWorkspaceID(manifest.WorkspaceID) || manifest.CreatedAt.IsZero() {
		return ErrInvalidMetadata
	}
	if len(manifest.Entries) > maxMetadataEntries {
		return ErrMetadataTooLarge
	}
	if requireDigest && !validDigest(manifest.IntegrityDigest) {
		return ErrInvalidMetadata
	}
	seen := make(map[string]struct{}, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if !validRelativeMetadataPath(entry.Path) || entry.Size < 0 ||
			entry.Mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || entry.Mode&os.ModeType != 0 {
			return ErrInvalidMetadata
		}
		if _, exists := seen[entry.Path]; exists {
			return ErrInvalidMetadata
		}
		seen[entry.Path] = struct{}{}
		switch entry.State {
		case ManifestEntryPlanned:
			if entry.IdentityDigest != "" {
				return ErrInvalidMetadata
			}
		case ManifestEntryCreated:
			if !validDigest(entry.IdentityDigest) {
				return ErrInvalidMetadata
			}
		default:
			return ErrInvalidMetadata
		}
		switch entry.Type {
		case ManifestFile:
			if !validDigest(entry.Digest) {
				return ErrInvalidMetadata
			}
		case ManifestDirectory:
			if entry.Digest != "" || entry.Size != 0 {
				return ErrInvalidMetadata
			}
		case ManifestSymlink:
			if !validDigest(entry.Digest) {
				return ErrInvalidMetadata
			}
		default:
			return ErrInvalidMetadata
		}
	}
	return nil
}

func manifestDigest(manifest Manifest) (string, error) {
	manifest.IntegrityDigest = ""
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", ErrInvalidMetadata
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func strictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing JSON value")
	}
	return nil
}

func validRelativeMetadataPath(value string) bool {
	if value == "" || len(value) > maxMetadataPath || filepath.IsAbs(value) || strings.Contains(value, `\`) || strings.ContainsRune(value, 0) {
		return false
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	return clean == value && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

func equalDigest(left, right string) bool {
	leftBytes, leftErr := hex.DecodeString(left)
	rightBytes, rightErr := hex.DecodeString(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

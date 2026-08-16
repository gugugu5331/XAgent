package worktree

import (
	"strings"
	"testing"
	"time"
)

func TestManifestEntryStateAndIdentityAreStrict(t *testing.T) {
	base := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		WorkspaceID:   initializerWorkspaceID,
		CreatedAt:     time.Unix(100, 0).UTC(),
	}
	for _, testCase := range []struct {
		name    string
		entry   ManifestEntry
		wantErr bool
	}{
		{
			name: "planned",
			entry: ManifestEntry{Path: "config/local.yaml", Type: ManifestFile, State: ManifestEntryPlanned,
				Size: 1, Digest: strings.Repeat("a", 64)},
		},
		{
			name: "created",
			entry: ManifestEntry{Path: "config/local.yaml", Type: ManifestFile, State: ManifestEntryCreated,
				Size: 1, Digest: strings.Repeat("a", 64), IdentityDigest: strings.Repeat("b", 64)},
		},
		{
			name: "missing state",
			entry: ManifestEntry{Path: "config/local.yaml", Type: ManifestFile,
				Size: 1, Digest: strings.Repeat("a", 64), IdentityDigest: strings.Repeat("b", 64)},
			wantErr: true,
		},
		{
			name: "unknown state",
			entry: ManifestEntry{Path: "config/local.yaml", Type: ManifestFile, State: ManifestEntryState("unknown"),
				Size: 1, Digest: strings.Repeat("a", 64), IdentityDigest: strings.Repeat("b", 64)},
			wantErr: true,
		},
		{
			name: "planned with identity",
			entry: ManifestEntry{Path: "config/local.yaml", Type: ManifestFile, State: ManifestEntryPlanned,
				Size: 1, Digest: strings.Repeat("a", 64), IdentityDigest: strings.Repeat("b", 64)},
			wantErr: true,
		},
		{
			name: "created without identity",
			entry: ManifestEntry{Path: "config/local.yaml", Type: ManifestFile, State: ManifestEntryCreated,
				Size: 1, Digest: strings.Repeat("a", 64)},
			wantErr: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := base.Clone()
			manifest.Entries = []ManifestEntry{testCase.entry}
			encoded, err := EncodeManifest(manifest)
			if testCase.wantErr {
				if err == nil {
					t.Fatal("invalid ownership state encoded")
				}
				return
			}
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			decoded, err := DecodeManifest(encoded)
			if err != nil || len(decoded.Entries) != 1 || decoded.Entries[0] != testCase.entry {
				t.Fatalf("round trip = %#v, %v", decoded, err)
			}
		})
	}
}

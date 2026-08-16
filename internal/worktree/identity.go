package worktree

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
)

var workspaceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type RepositoryIdentity struct {
	Root      string `json:"root"`
	CommonDir string `json:"common_dir"`
	Digest    string `json:"digest"`
}

func NewRepositoryIdentity(root, commonDir string) (RepositoryIdentity, error) {
	canonicalRoot, err := canonicalDirectory(root)
	if err != nil {
		return RepositoryIdentity{}, ErrIdentityMismatch
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(canonicalRoot, commonDir)
	}
	canonicalCommon, err := canonicalDirectory(commonDir)
	if err != nil {
		return RepositoryIdentity{}, ErrIdentityMismatch
	}
	objectIdentity, err := repositoryDirectoryObjectIdentity(canonicalCommon)
	if err != nil || len(objectIdentity) == 0 {
		return RepositoryIdentity{}, ErrIdentityMismatch
	}
	digestInput := append([]byte("xagent-repository-v2\x00"), objectIdentity...)
	digest := sha256.Sum256(digestInput)
	return RepositoryIdentity{Root: canonicalRoot, CommonDir: canonicalCommon, Digest: hex.EncodeToString(digest[:])}, nil
}

func (i RepositoryIdentity) Equal(other RepositoryIdentity) bool {
	return validDigest(i.Digest) && i.Digest == other.Digest
}

func GenerateWorkspaceID(reader io.Reader) (string, error) {
	if reader == nil {
		reader = rand.Reader
	}
	buffer := make([]byte, 16)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return "", fmt.Errorf("generate workspace identity: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func GenerateOwnerID(reader io.Reader) (string, error) { return GenerateWorkspaceID(reader) }

func ValidWorkspaceID(value string) bool { return workspaceIDPattern.MatchString(value) }

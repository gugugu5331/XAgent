package repoaudit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	binaryTypeFindingMessage = "binary content is not in the exact allowlist"
)

func binaryFindings(name string, input io.Reader, policy Policy) ([]Finding, error) {
	if input == nil {
		return nil, errors.New("binary scan input is nil")
	}
	hash := sha256.New()
	counter := new(byteCounter)
	reader := bufio.NewReader(io.TeeReader(input, io.MultiWriter(hash, counter)))
	binary := false
	for {
		r, size, err := reader.ReadRune()
		switch {
		case err == nil:
			if r == 0 || r == utf8.RuneError && size == 1 {
				binary = true
			}
		case errors.Is(err, io.EOF):
			if !binary {
				return nil, nil
			}
			digest := hex.EncodeToString(hash.Sum(nil))
			if counter.bytes > MaxBinaryBytes {
				return []Finding{newBinaryFinding(name, fmt.Sprintf("binary content exceeds %d bytes (size=%d)", MaxBinaryBytes, counter.bytes))}, nil
			}
			if binaryIdentityAllowed(policy, name, digest, counter.bytes) {
				return nil, nil
			}
			return []Finding{newBinaryFinding(name, binaryTypeFindingMessage)}, nil
		default:
			return nil, errors.New("binary scan input read failed")
		}
	}
}

type byteCounter struct {
	bytes int64
}

func (c *byteCounter) Write(data []byte) (int, error) {
	if int64(len(data)) > int64(^uint64(0)>>1)-c.bytes {
		return 0, errors.New("binary scan byte count overflow")
	}
	c.bytes += int64(len(data))
	return len(data), nil
}

func binaryIdentityAllowed(policy Policy, name, digest string, size int64) bool {
	for _, declaration := range policy.BinaryAllowlist {
		if declaration.Path == name && declaration.ContentSHA256 == digest && declaration.Bytes == size {
			return true
		}
	}
	return false
}

func newBinaryFinding(name, message string) Finding {
	return Finding{
		RuleID:   RuleBinary,
		Path:     name,
		Severity: SeverityError,
		Message:  message,
	}
}

package permission

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"

	"xagent/internal/safefs"
)

const bashIdentityDomain = "xagent.permission.bash-identity"

// BashIdentityInput contains the exact execution context for one Bash call.
// RawCommand is deliberately a byte slice: no trimming, whitespace folding,
// display sanitization, or text re-encoding is allowed before it is bound.
type BashIdentityInput struct {
	Shell             string
	WorkingDirectory  safefs.Identity
	RawCommand        []byte
	EnvironmentDigest [32]byte
}

// NewBashIdentity binds raw shell semantics independently from the normalized
// text used for rule matching and display.
func NewBashIdentity(input BashIdentityInput) (CallIdentity, error) {
	if strings.TrimSpace(input.Shell) == "" {
		return CallIdentity{}, errors.New("bash identity shell is empty")
	}
	workingDirectory, err := input.WorkingDirectory.MarshalBinary()
	if err != nil {
		return CallIdentity{}, errors.New("bash identity working directory is invalid")
	}

	var encoded bytes.Buffer
	writeIdentityUint16(&encoded, callIdentityVersion)
	writeIdentityBytes(&encoded, []byte(bashIdentityDomain))
	writeIdentityBytes(&encoded, []byte(input.Shell))
	writeIdentityBytes(&encoded, workingDirectory)
	writeIdentityBytes(&encoded, input.RawCommand)
	writeIdentityBytes(&encoded, input.EnvironmentDigest[:])

	return CallIdentity{
		version: callIdentityVersion,
		digest:  sha256.Sum256(encoded.Bytes()),
	}, nil
}

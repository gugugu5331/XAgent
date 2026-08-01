package permission

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"xagent/internal/safefs"
)

const callIdentityVersion uint16 = 1

const callIdentityDomain = "xagent.permission.call-identity"

// CallIdentity is the opaque, comparable identity of one validated tool call.
// The version is kept outside the digest so future ticket verification can
// reject identities that use an unsupported encoding.
type CallIdentity struct {
	version uint16
	digest  [32]byte
}

// CallIdentityInput contains only execution-relevant call state. In
// particular, call IDs, display text, redacted summaries, and rule text do not
// participate in the identity.
type CallIdentityInput struct {
	ToolName           string
	CanonicalArguments []byte
	WorkingDirectory   *safefs.Identity
	ResourceBindings   []safefs.Binding
	EnvironmentDigest  *[32]byte
	TargetDigest       *[32]byte
}

// NewCallIdentity creates a versioned digest from a canonical, length-prefixed
// encoding. JSON object keys and resource bindings are ordered before hashing
// so caller enumeration order cannot affect the result.
func NewCallIdentity(input CallIdentityInput) (CallIdentity, error) {
	if strings.TrimSpace(input.ToolName) == "" {
		return CallIdentity{}, errors.New("call identity tool name is empty")
	}
	arguments, err := canonicalCallArguments(input.CanonicalArguments)
	if err != nil {
		return CallIdentity{}, err
	}

	var workingDirectory []byte
	if input.WorkingDirectory != nil {
		workingDirectory, err = input.WorkingDirectory.MarshalBinary()
		if err != nil {
			return CallIdentity{}, errors.New("call identity working directory is invalid")
		}
	}
	bindings, err := canonicalCallBindings(input.ResourceBindings)
	if err != nil {
		return CallIdentity{}, err
	}

	var encoded bytes.Buffer
	writeIdentityUint16(&encoded, callIdentityVersion)
	writeIdentityBytes(&encoded, []byte(callIdentityDomain))
	writeIdentityBytes(&encoded, []byte(input.ToolName))
	writeIdentityBytes(&encoded, arguments)
	writeIdentityOptionalBytes(&encoded, input.WorkingDirectory != nil, workingDirectory)
	writeIdentityUint64(&encoded, uint64(len(bindings)))
	for _, binding := range bindings {
		writeIdentityBytes(&encoded, binding)
	}
	writeIdentityOptionalDigest(&encoded, input.EnvironmentDigest)
	writeIdentityOptionalDigest(&encoded, input.TargetDigest)

	return CallIdentity{
		version: callIdentityVersion,
		digest:  sha256.Sum256(encoded.Bytes()),
	}, nil
}

func canonicalCallArguments(input []byte) ([]byte, error) {
	if len(input) == 0 || !utf8.Valid(input) {
		return nil, errors.New("call identity arguments are invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("call identity arguments are invalid")
	}
	arguments, ok := value.(map[string]any)
	if !ok || arguments == nil {
		return nil, errors.New("call identity arguments must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("call identity arguments are invalid")
	}
	canonical, err := json.Marshal(arguments)
	if err != nil {
		return nil, errors.New("call identity arguments are invalid")
	}
	return canonical, nil
}

func canonicalCallBindings(input []safefs.Binding) ([][]byte, error) {
	bindings := make([][]byte, len(input))
	for index, binding := range input {
		encoded, err := binding.MarshalBinary()
		if err != nil {
			return nil, errors.New("call identity resource binding is invalid")
		}
		bindings[index] = encoded
	}
	sort.Slice(bindings, func(first, second int) bool {
		return bytes.Compare(bindings[first], bindings[second]) < 0
	})
	for index := 1; index < len(bindings); index++ {
		if bytes.Equal(bindings[index-1], bindings[index]) {
			return nil, errors.New("call identity contains a duplicate resource binding")
		}
	}
	return bindings, nil
}

func writeIdentityOptionalDigest(encoded *bytes.Buffer, digest *[32]byte) {
	if digest == nil {
		writeIdentityOptionalBytes(encoded, false, nil)
		return
	}
	writeIdentityOptionalBytes(encoded, true, digest[:])
}

func writeIdentityOptionalBytes(encoded *bytes.Buffer, present bool, value []byte) {
	if !present {
		encoded.WriteByte(0)
		return
	}
	encoded.WriteByte(1)
	writeIdentityBytes(encoded, value)
}

func writeIdentityBytes(encoded *bytes.Buffer, value []byte) {
	writeIdentityUint64(encoded, uint64(len(value)))
	_, _ = encoded.Write(value)
}

func writeIdentityUint16(encoded *bytes.Buffer, value uint16) {
	var buffer [2]byte
	binary.BigEndian.PutUint16(buffer[:], value)
	_, _ = encoded.Write(buffer[:])
}

func writeIdentityUint64(encoded *bytes.Buffer, value uint64) {
	var buffer [8]byte
	binary.BigEndian.PutUint64(buffer[:], value)
	_, _ = encoded.Write(buffer[:])
}

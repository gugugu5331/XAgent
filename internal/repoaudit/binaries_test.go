package repoaudit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestBinarySizeBoundaryAtOneMiB(t *testing.T) {
	t.Parallel()

	atLimit := bytes.Repeat([]byte{0}, int(MaxBinaryBytes))
	policy := policyAllowingBinary("fixture.bin", atLimit)
	findings, err := binaryFindings("fixture.bin", bytes.NewReader(atLimit), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("allowlisted binary at limit findings = %#v", findings)
	}

	overLimit := append(append([]byte(nil), atLimit...), 0)
	findings, err = binaryFindings("fixture.bin", bytes.NewReader(overLimit), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !strings.Contains(findings[0].Message, "size=1048577") {
		t.Fatalf("binary over limit findings = %#v", findings)
	}
}

func TestTextIsNotSubjectToBinarySizeLimit(t *testing.T) {
	t.Parallel()

	text := bytes.Repeat([]byte("a"), int(MaxBinaryBytes)+1)
	findings, err := binaryFindings("large.txt", bytes.NewReader(text), Policy{Version: PolicyVersion, MaxBinaryBytes: MaxBinaryBytes})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("large UTF-8 text was subject to binary limit: %#v", findings)
	}
}

func TestUnlistedBinaryIsRejected(t *testing.T) {
	t.Parallel()

	findings, err := binaryFindings("small.bin", bytes.NewReader([]byte{'a', 0, 'b'}), Policy{Version: PolicyVersion, MaxBinaryBytes: MaxBinaryBytes})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].RuleID != RuleBinary || findings[0].Message != binaryTypeFindingMessage {
		t.Fatalf("unlisted binary findings = %#v", findings)
	}
}

func TestBinaryAllowlistCannotBypassOtherRules(t *testing.T) {
	t.Parallel()

	const canary = "binary-secret-canary-274915"
	const name = ".mewcode/memory/private.bin"
	content := []byte("OPENAI_API_KEY=" + canary + "\x00")
	policy := policyAllowingBinary(name, content)
	binaryResults, err := binaryFindings(name, bytes.NewReader(content), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(binaryResults) != 0 {
		t.Fatalf("exact binary identity was not allowed: %#v", binaryResults)
	}
	privateResults := privatePathFindings([]Entry{{Path: name, Type: "blob"}})
	secretResults, err := secretFindings(name, bytes.NewReader(content), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(privateResults) != 1 || len(secretResults) != 1 {
		t.Fatalf("binary allowlist bypassed another rule: private=%#v secret-count=%d", privateResults, len(secretResults))
	}
	if strings.Contains(secretResults[0].Message, canary) {
		t.Fatal("secret finding leaked binary canary")
	}
}

func policyAllowingBinary(name string, content []byte) Policy {
	digest := sha256.Sum256(content)
	return Policy{
		Version:        PolicyVersion,
		MaxBinaryBytes: MaxBinaryBytes,
		BinaryAllowlist: []BinaryDeclaration{{
			Path:          name,
			ContentSHA256: hex.EncodeToString(digest[:]),
			Bytes:         int64(len(content)),
		}},
	}
}

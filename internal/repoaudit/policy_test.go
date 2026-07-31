package repoaudit

import (
	"reflect"
	"strings"
	"testing"
)

func TestPolicyRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"unknown":   "version: 1\nmax_binary_bytes: 1048576\nbinary_allowlist: []\ngitlinks: []\nfixture_exemptions: []\nunknown: true\n",
		"duplicate": "version: 1\nversion: 1\nmax_binary_bytes: 1048576\nbinary_allowlist: []\ngitlinks: []\nfixture_exemptions: []\n",
		"null":      "version: 1\nmax_binary_bytes: 1048576\nbinary_allowlist: []\ngitlinks: null\nfixture_exemptions: []\n",
		"second":    "version: 1\nmax_binary_bytes: 1048576\nbinary_allowlist: []\ngitlinks: []\nfixture_exemptions: []\n---\n{}\n",
	}
	for name, input := range tests {
		name, input := name, input
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParsePolicy([]byte(input)); err == nil {
				t.Fatal("ParsePolicy unexpectedly accepted invalid input")
			}
		})
	}
}

func TestFixtureExemptionRequiresDigest(t *testing.T) {
	t.Parallel()

	valid := "version: 1\nmax_binary_bytes: 1048576\nbinary_allowlist: []\ngitlinks: []\nfixture_exemptions:\n  - path: internal/repoaudit/testdata/secret.txt\n    rule_id: secret\n    content_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
	policy, err := ParsePolicy([]byte(valid))
	if err != nil {
		t.Fatalf("ParsePolicy(valid): %v", err)
	}
	if got := len(policy.FixtureExemptions); got != 1 {
		t.Fatalf("fixture exemption count = %d, want 1", got)
	}

	invalid := []string{
		strings.Replace(valid, "path: internal/repoaudit/testdata/secret.txt", "path: ''", 1),
		strings.Replace(valid, "rule_id: secret", "rule_id: ''", 1),
		strings.Replace(valid, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "ABCDEF", 1),
	}
	for index, input := range invalid {
		if _, err := ParsePolicy([]byte(input)); err == nil {
			t.Fatalf("invalid fixture exemption %d was accepted", index)
		}
	}
}

func TestFindingNeverStoresMatchedText(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(Finding{})
	for _, forbidden := range []string{"Match", "Matched", "Text", "Content", "Value", "Secret", "Bytes"} {
		if _, ok := typ.FieldByName(forbidden); ok {
			t.Fatalf("Finding exposes forbidden matched-text field %q", forbidden)
		}
	}
	finding := Finding{
		RuleID:   RuleSecret,
		Path:     "fixture.txt",
		Line:     1,
		Severity: SeverityError,
		Message:  "possible credential",
	}
	if strings.Contains(strings.ToLower(finding.Message), "token-") {
		t.Fatal("Finding message retained a matched credential")
	}
}

func TestPolicyFixesOneMiBBinaryLimit(t *testing.T) {
	t.Parallel()

	valid := "version: 1\nmax_binary_bytes: 1048576\nbinary_allowlist: []\ngitlinks: []\nfixture_exemptions: []\n"
	policy, err := ParsePolicy([]byte(valid))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	if len(policy.Gitlinks) != 0 {
		t.Fatalf("gitlinks = %v, want empty", policy.Gitlinks)
	}
	if MaxBinaryBytes != 1048576 {
		t.Fatalf("MaxBinaryBytes = %d, want 1048576", MaxBinaryBytes)
	}
	if policy.MaxBinaryBytes != MaxBinaryBytes {
		t.Fatalf("policy max_binary_bytes = %d, want %d", policy.MaxBinaryBytes, MaxBinaryBytes)
	}
	for _, replacement := range []string{"1048575", "1048577", "0"} {
		if _, err := ParsePolicy([]byte(strings.Replace(valid, "1048576", replacement, 1))); err == nil {
			t.Fatalf("mutable max_binary_bytes %s was accepted", replacement)
		}
	}
}

func TestBinaryAllowlistRequiresExactIdentity(t *testing.T) {
	t.Parallel()

	valid := "version: 1\nmax_binary_bytes: 1048576\nbinary_allowlist:\n  - path: internal/repoaudit/testdata/image.bin\n    content_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n    bytes: 128\ngitlinks: []\nfixture_exemptions: []\n"
	policy, err := ParsePolicy([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.BinaryAllowlist) != 1 || policy.BinaryAllowlist[0].Bytes != 128 {
		t.Fatalf("binary allowlist = %#v", policy.BinaryAllowlist)
	}
	invalid := []string{
		strings.Replace(valid, "path: internal/repoaudit/testdata/image.bin", "path: ''", 1),
		strings.Replace(valid, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "ABCDEF", 1),
		strings.Replace(valid, "bytes: 128", "bytes: 0", 1),
		strings.Replace(valid, "bytes: 128", "bytes: 1048577", 1),
		strings.Replace(valid, "gitlinks: []", "binary_allowlist:\n  - path: internal/repoaudit/testdata/image.bin\n    content_sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n    bytes: 128\ngitlinks: []", 1),
	}
	for index, input := range invalid {
		if _, err := ParsePolicy([]byte(input)); err == nil {
			t.Fatalf("invalid binary allowlist %d was accepted", index)
		}
	}
}

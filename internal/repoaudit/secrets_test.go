package repoaudit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestSecretCanaryFailsWithoutLeak(t *testing.T) {
	t.Parallel()

	const canary = "xagent-canary-credential-8f4d2a91"
	content := "ordinary line\nOPENAI_API_KEY=" + canary + "\ntrailing line\n"
	findings, err := secretFindings("config.fixture", strings.NewReader(content), Policy{Version: PolicyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("secret finding count = %d, want 1", len(findings))
	}
	finding := findings[0]
	if finding.RuleID != RuleSecret || finding.Path != "config.fixture" || finding.Line != 2 || finding.Severity != SeverityError || finding.Message != secretFindingMessage {
		t.Fatalf("unexpected secret finding: %#v", finding)
	}
	if rendered := fmt.Sprintf("%#v", findings); strings.Contains(rendered, canary) {
		t.Fatal("secret finding retained or rendered the credential canary")
	}
}

func TestExactFixtureExemption(t *testing.T) {
	t.Parallel()

	const name = "internal/repoaudit/testdata/secret.fixture"
	content := []byte("GITHUB_TOKEN=ghp_fixture_canary_12345678\n")
	digest := sha256.Sum256(content)
	policy := Policy{
		Version: PolicyVersion,
		FixtureExemptions: []FixtureExemption{{
			Path:          name,
			RuleID:        RuleSecret,
			ContentSHA256: hex.EncodeToString(digest[:]),
		}},
	}
	findings, err := secretFindings(name, strings.NewReader(string(content)), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("exact fixture exemption produced findings: %#v", findings)
	}

	changed := append([]byte(nil), content...)
	changed[len(changed)-2] = '9'
	findings, err = secretFindings(name, strings.NewReader(string(changed)), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("changed fixture finding count = %d, want 1", len(findings))
	}
	findings, err = secretFindings("different.fixture", strings.NewReader(string(content)), policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 {
		t.Fatalf("different-path fixture finding count = %d, want 1", len(findings))
	}
}

func TestSecretScannerRejectsCodeAndPlaceholderFalsePositives(t *testing.T) {
	t.Parallel()

	content := strings.Join([]string{
		`token := source`,
		`api_key: your-api-key`,
		`password = os.Getenv("PASSWORD")`,
		`"authorization": "Bearer"`,
	}, "\n")
	findings, err := secretFindings("ordinary.go", strings.NewReader(content), Policy{Version: PolicyVersion})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("ordinary code or placeholders produced %d secret findings", len(findings))
	}
}

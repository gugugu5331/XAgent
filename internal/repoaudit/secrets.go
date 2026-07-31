package repoaudit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"strings"
)

const (
	secretScanBufferBytes       = 64 << 10
	secretScanOverlapBytes      = 512
	maxSecretFindingsPerContent = 100
	secretFindingMessage        = "possible credential material"
)

var secretIndicators = []*regexp.Regexp{
	regexp.MustCompile(`(^|[^A-Za-z0-9_-])sk-ant-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(^|[^A-Za-z0-9_-])sk-(?:proj-)?[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`(^|[^A-Za-z0-9_])ghp_[A-Za-z0-9_]{8,}`),
	regexp.MustCompile(`(^|[^A-Za-z0-9_])github_pat_[A-Za-z0-9_]{16,}`),
	regexp.MustCompile(`(^|[^A-Z0-9])AKIA[A-Z0-9]{16}([^A-Z0-9]|$)`),
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)authorization\s*[:=]\s*bearer\s+[A-Za-z0-9_.+-]{16,}`),
}

var secretAssignmentIndicator = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])["']?(api[_-]?key|apikey|access[_-]?token|refresh[_-]?token|token|secret|password|cookie|set-cookie|authorization|credential|aws_secret_access_key|github_token|anthropic_api_key|openai_api_key|x-api-key)["']?\s*(?::=|=|:)\s*([^\s,;]+)`)

// secretFindings scans incrementally with fixed memory. It never stores a
// matched substring; the only content-derived value retained is the complete
// SHA-256 used to evaluate an exact fixture exemption.
func secretFindings(name string, input io.Reader, policy Policy) ([]Finding, error) {
	if input == nil {
		return nil, errors.New("secret scan input is nil")
	}
	digest := sha256.New()
	reader := bufio.NewReaderSize(io.TeeReader(input, digest), secretScanBufferBytes)
	findings := make([]Finding, 0)
	line := 1
	overlap := make([]byte, 0, secretScanOverlapBytes)
	lineMatched := false

	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			window := make([]byte, 0, len(overlap)+len(fragment))
			window = append(window, overlap...)
			window = append(window, fragment...)
			if !lineMatched && containsSecretIndicator(window) {
				if len(findings) < maxSecretFindingsPerContent {
					findings = append(findings, Finding{
						RuleID:   RuleSecret,
						Path:     name,
						Line:     line,
						Severity: SeverityError,
						Message:  secretFindingMessage,
					})
				}
				lineMatched = true
			}
			if fragment[len(fragment)-1] == '\n' {
				line++
				lineMatched = false
				overlap = overlap[:0]
			} else {
				overlap = retainSecretOverlap(overlap, fragment)
			}
		}
		switch {
		case err == nil:
			continue
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			encodedDigest := hex.EncodeToString(digest.Sum(nil))
			if hasExactFixtureExemption(policy, name, RuleSecret, encodedDigest) {
				return nil, nil
			}
			return findings, nil
		default:
			return nil, errors.New("secret scan input read failed")
		}
	}
}

func containsSecretIndicator(data []byte) bool {
	for _, indicator := range secretIndicators {
		if indicator.Match(data) {
			return true
		}
	}
	for _, indexes := range secretAssignmentIndicator.FindAllSubmatchIndex(data, -1) {
		const valueGroup = 3
		start, end := indexes[valueGroup*2], indexes[valueGroup*2+1]
		if start >= 0 && likelySecretAssignmentValue(data[start:end]) {
			return true
		}
	}
	return false
}

func likelySecretAssignmentValue(value []byte) bool {
	if terminator := bytes.IndexByte(value, 0); terminator >= 0 {
		value = value[:terminator]
	}
	value = bytes.Trim(value, `"'`+"`")
	if len(value) < 12 || bytes.IndexAny(value, `$<>{}[]()\\/:`) >= 0 {
		return false
	}
	lower := strings.ToLower(string(value))
	for _, marker := range []string{"your-", "your_", "example", "placeholder", "fixture", "redacted", "changeme", "change-me", "dummy", "sample"} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	var lowerLetters, upperLetters, digits, punctuation int
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
			lowerLetters++
		case character >= 'A' && character <= 'Z':
			upperLetters++
		case character >= '0' && character <= '9':
			digits++
		case bytes.ContainsRune([]byte("_-.+="), rune(character)):
			punctuation++
		default:
			return false
		}
	}
	letters := lowerLetters + upperLetters
	return letters > 0 && (digits > 0 || punctuation > 0 || lowerLetters > 0 && upperLetters > 0)
}

func retainSecretOverlap(previous, fragment []byte) []byte {
	combinedLength := len(previous) + len(fragment)
	keep := combinedLength
	if keep > secretScanOverlapBytes {
		keep = secretScanOverlapBytes
	}
	result := make([]byte, keep)
	start := combinedLength - keep
	for index := 0; index < keep; index++ {
		position := start + index
		if position < len(previous) {
			result[index] = previous[position]
		} else {
			result[index] = fragment[position-len(previous)]
		}
	}
	return result
}

func hasExactFixtureExemption(policy Policy, name string, rule RuleID, digest string) bool {
	for _, exemption := range policy.FixtureExemptions {
		if exemption.Path == name && exemption.RuleID == rule && exemption.ContentSHA256 == digest {
			return true
		}
	}
	return false
}

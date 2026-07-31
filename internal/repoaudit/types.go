// Package repoaudit implements read-only checks over explicit Git and
// filesystem sources. Findings deliberately contain no matched source text.
package repoaudit

// RuleID is a stable machine-readable audit rule identifier.
type RuleID string

const (
	RulePrivatePath RuleID = "private-path"
	RuleSecret      RuleID = "secret"
	RuleGitlink     RuleID = "gitlink"
	RuleBinary      RuleID = "binary"
)

// Severity is the closed severity set emitted by repository audit rules.
type Severity string

const (
	SeverityError Severity = "error"
)

// Finding identifies a rule violation without retaining the matched bytes.
// Message must be a fixed, non-sensitive diagnostic owned by the rule.
type Finding struct {
	RuleID   RuleID
	Path     string
	Line     int
	Severity Severity
	Message  string
}

// Report is the deterministic result of auditing an explicit source.
type Report struct {
	Checked  int
	Findings []Finding
}

// Passed reports whether no rule findings were produced.
func (r Report) Passed() bool {
	return len(r.Findings) == 0
}

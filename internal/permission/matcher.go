package permission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"xagent/internal/matcher"
)

type Call struct {
	ID            string
	Name          string
	ArgumentsJSON string
}

type NormalizedCall struct {
	Call             Call
	Arguments        map[string]any
	ProjectRoot      string
	ReadRoots        []string
	RawCommand       string
	Command          string
	ComplexShell     bool
	Path             string
	OriginalPath     string
	RuleValue        string
	FingerprintValue string
}

func NormalizeCall(call Call, projectRoot string) (NormalizedCall, error) {
	return NormalizeCallWithReadRoots(call, projectRoot, nil)
}

func NormalizeCallWithReadRoots(call Call, projectRoot string, readRoots []string) (NormalizedCall, error) {
	arguments, err := decodeArguments(call.ArgumentsJSON)
	if err != nil {
		return NormalizedCall{}, err
	}
	return normalizeArguments(call, arguments, projectRoot, readRoots)
}

// NormalizeArguments normalizes an already parsed argument map. The map is
// intentionally retained, rather than round-tripped through JSON, so callers
// can preserve json.Number and share one representation across safety stages.
func NormalizeArguments(call Call, arguments map[string]any, context Context) (NormalizedCall, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	return normalizeArguments(call, arguments, context.ProjectRoot, context.ReadRoots)
}

func normalizeArguments(call Call, arguments map[string]any, projectRoot string, readRoots []string) (NormalizedCall, error) {
	normalized := NormalizedCall{Call: call, Arguments: arguments, ProjectRoot: projectRoot, ReadRoots: append([]string(nil), readRoots...)}
	switch call.Name {
	case "Bash":
		command, _ := arguments["command"].(string)
		normalized.RawCommand = strings.TrimSpace(command)
		normalized.Command = NormalizeCommand(command)
		normalized.ComplexShell = IsComplexShell(normalized.RawCommand)
		normalized.RuleValue = normalized.Command
		normalized.FingerprintValue = normalized.Command
	case "Read", "Glob", "Grep":
		pathValue, original, err := normalizeReadPathArgument(call.Name, arguments, projectRoot, readRoots)
		if err != nil {
			return NormalizedCall{}, err
		}
		normalized.Path = pathValue
		normalized.OriginalPath = original
		normalized.RuleValue = pathValue
		normalized.FingerprintValue = pathValue
	case "Write", "Edit":
		pathValue, original, err := normalizePathArgument(call.Name, arguments, projectRoot)
		if err != nil {
			return NormalizedCall{}, err
		}
		normalized.Path = pathValue
		normalized.OriginalPath = original
		normalized.RuleValue = pathValue
		normalized.FingerprintValue = pathValue
	default:
		normalized.RuleValue = strings.TrimSpace(call.ArgumentsJSON)
		if strings.HasPrefix(call.Name, "mcp__") {
			server, remote := mcpNameParts(call.Name)
			normalized.FingerprintValue = fmt.Sprintf("registered=%s;server=%s;tool=%s;args=%s", call.Name, server, remote, argumentHash(arguments))
		} else {
			normalized.FingerprintValue = normalized.RuleValue
		}
	}
	return normalized, nil
}

func decodeArguments(raw string) (map[string]any, error) {
	arguments := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return arguments, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&arguments); err != nil {
		return nil, err
	}
	if arguments == nil { // Preserve the legacy treatment of JSON null as {}.
		arguments = map[string]any{}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("additional JSON value")
		}
		return nil, fmt.Errorf("trailing tool arguments: %w", err)
	}
	return arguments, nil
}

func NormalizeCommand(command string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(command)), " ")
}

func IsComplexShell(command string) bool {
	if command == "" {
		return false
	}
	patterns := []string{
		`&&`, `\|\|`, `;`, `\|`, "`", `\$\(`, `[<>]`, `<<`, `\r|\n`,
		`(^|\s)(sh|bash|zsh)\s+-(c|lc)(\s|$)`, `(^|\s)xargs(\s|$)`, `(^|\s)find\s+.*\s-exec(\s|$)`, `(^|\s)eval(\s|$)`,
	}
	for _, pattern := range patterns {
		if regexp.MustCompile(pattern).MatchString(command) {
			return true
		}
	}
	return false
}

func MatchRule(rule Rule, normalized NormalizedCall) (bool, error) {
	if rule.Tool != normalized.Call.Name {
		return false, nil
	}
	if rule.Tool == "Bash" && normalized.ComplexShell && rule.MatchType == string(MatchGlob) {
		return false, nil
	}
	value, err := ruleValue(rule, normalized)
	if err != nil {
		return false, err
	}
	if rule.MatchType == string(MatchExact) {
		return matcher.MatchExact(value, normalizePattern(rule.Tool, rule.Pattern)), nil
	}
	return matcher.MatchGlob(filepath.ToSlash(value), filepath.ToSlash(normalizePattern(rule.Tool, rule.Pattern)))
}

func ruleValue(rule Rule, normalized NormalizedCall) (string, error) {
	if rule.PathParam != "" && rule.Tool != "Bash" {
		return normalizePathForParam(rule.Tool, normalized.Arguments, normalized.ProjectRoot, normalized.ReadRoots, rule.PathParam)
	}
	return normalized.RuleValue, nil
}

func normalizePathForParam(toolName string, arguments map[string]any, projectRoot string, readRoots []string, pathParam string) (string, error) {
	value := "."
	if raw, ok := arguments[pathParam].(string); ok && strings.TrimSpace(raw) != "" {
		value = raw
	}
	var resolved PathResolution
	var err error
	if toolName == "Read" || toolName == "Glob" || toolName == "Grep" {
		resolved, err = ResolveReadPath(projectRoot, readRoots, value)
	} else {
		resolved, err = ResolveProjectPath(projectRoot, value)
	}
	if err != nil {
		return "", err
	}
	return resolved.Relative, nil
}

func FindRule(rules []Rule, normalized NormalizedCall) (*Rule, bool, error) {
	var best *Rule
	bestScore := ruleScore{}
	for index := range rules {
		rule := rules[index]
		matched, err := MatchRule(rule, normalized)
		if err != nil {
			return nil, false, err
		}
		if !matched {
			continue
		}
		score := scoreRule(rule, index)
		if best == nil || score.betterThan(bestScore) {
			best = &rules[index]
			bestScore = score
		}
	}
	if best == nil {
		return nil, false, nil
	}
	return best, true, nil
}

type ruleScore struct {
	deny      bool
	exact     bool
	literal   int
	wildcards int
	order     int
}

func scoreRule(rule Rule, index int) ruleScore {
	return ruleScore{
		deny:      rule.Effect == string(EffectDeny),
		exact:     rule.MatchType == string(MatchExact),
		literal:   literalCount(rule.Pattern),
		wildcards: strings.Count(rule.Pattern, "*") + strings.Count(rule.Pattern, "?"),
		order:     index,
	}
}

func (s ruleScore) betterThan(other ruleScore) bool {
	if s.deny != other.deny {
		return s.deny
	}
	if s.exact != other.exact {
		return s.exact
	}
	if s.literal != other.literal {
		return s.literal > other.literal
	}
	if s.wildcards != other.wildcards {
		return s.wildcards < other.wildcards
	}
	return s.order > other.order
}

func literalCount(pattern string) int {
	count := 0
	for _, r := range pattern {
		if r != '*' && r != '?' {
			count++
		}
	}
	return count
}

func normalizePattern(toolName string, pattern string) string {
	if toolName == "Bash" {
		return NormalizeCommand(pattern)
	}
	return filepath.ToSlash(strings.TrimSpace(pattern))
}

func Fingerprint(normalized NormalizedCall) string {
	return fmt.Sprintf("%s:%s", normalized.Call.Name, normalized.FingerprintValue)
}

func argumentHash(arguments map[string]any) string {
	data, err := json.Marshal(arguments)
	if err != nil {
		data = []byte("{}")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func mcpNameParts(name string) (string, string) {
	trimmed := strings.TrimPrefix(name, "mcp__")
	parts := strings.SplitN(trimmed, "__", 2)
	if len(parts) != 2 {
		return trimmed, ""
	}
	return parts[0], parts[1]
}

package repoaudit

import (
	"context"
	"errors"
	"io"
	"sort"
)

const MaxReportFindings = 1000

// AuditExitClass distinguishes success, policy findings and audit failure.
type AuditExitClass uint8

const (
	AuditExitPass AuditExitClass = iota
	AuditExitFindings
	AuditExitError
)

type Auditor struct {
	Policy Policy
}

func (a Auditor) Audit(ctx context.Context, source Source) (Report, error) {
	if ctx == nil {
		return Report{}, errors.New("repoaudit context is nil")
	}
	if source == nil {
		return Report{}, errors.New("repoaudit source is nil")
	}
	if err := a.Policy.validate(); err != nil {
		return Report{}, err
	}
	entries, err := source.Entries(ctx)
	if err != nil {
		return Report{}, errors.New("enumerate repository source failed")
	}
	entries = append([]Entry(nil), entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	for index, entry := range entries {
		if entry.Stage != 0 {
			return Report{}, errors.New("repository source contains a non-zero index stage")
		}
		if index > 0 && entries[index-1].Path == entry.Path {
			return Report{}, errors.New("repository source contains a duplicate path")
		}
	}

	findings := privatePathFindings(entries)
	gitmodules, gitmodulesPresent, err := readGitmodules(ctx, source, entries)
	if err != nil {
		return Report{}, err
	}
	gitlinkResults, err := gitlinkFindings(entries, gitmodules, gitmodulesPresent, a.Policy)
	if err != nil {
		return Report{}, err
	}
	findings = append(findings, gitlinkResults...)

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return Report{}, errors.New("repoaudit canceled")
		}
		if entry.Mode == 0160000 || entry.Type == "commit" || entry.Type == "directory" {
			continue
		}
		binaryResults, err := scanEntry(ctx, source, entry, func(reader io.Reader) ([]Finding, error) {
			return binaryFindings(entry.Path, reader, a.Policy)
		})
		if err != nil {
			return Report{}, err
		}
		findings = append(findings, binaryResults...)
		secretResults, err := scanEntry(ctx, source, entry, func(reader io.Reader) ([]Finding, error) {
			return secretFindings(entry.Path, reader, a.Policy)
		})
		if err != nil {
			return Report{}, err
		}
		findings = append(findings, secretResults...)
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		if rank := findingRuleRank(findings[i].RuleID) - findingRuleRank(findings[j].RuleID); rank != 0 {
			return rank < 0
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].Message < findings[j].Message
	})
	if len(findings) > MaxReportFindings {
		findings = findings[:MaxReportFindings]
	}
	return Report{Checked: len(entries), Findings: findings}, nil
}

func ClassifyAudit(report Report, err error) AuditExitClass {
	if err != nil {
		return AuditExitError
	}
	if !report.Passed() {
		return AuditExitFindings
	}
	return AuditExitPass
}

func readGitmodules(ctx context.Context, source Source, entries []Entry) ([]byte, bool, error) {
	index := sort.Search(len(entries), func(index int) bool { return entries[index].Path >= ".gitmodules" })
	if index == len(entries) || entries[index].Path != ".gitmodules" {
		return nil, false, nil
	}
	entry := entries[index]
	var gitmodules []byte
	results, err := scanEntry(ctx, source, entry, func(reader io.Reader) ([]Finding, error) {
		data, err := io.ReadAll(io.LimitReader(reader, maxGitmodulesBytes+1))
		if err != nil {
			return nil, errors.New("read .gitmodules failed")
		}
		if len(data) > maxGitmodulesBytes {
			return nil, errors.New(".gitmodules exceeds its size limit")
		}
		gitmodules = append([]byte(nil), data...)
		return nil, nil
	})
	_ = results
	if err != nil {
		return nil, false, err
	}
	return gitmodules, true, nil
}

func scanEntry(ctx context.Context, source Source, entry Entry, scan func(io.Reader) ([]Finding, error)) ([]Finding, error) {
	reader, err := source.Open(ctx, entry)
	if err != nil {
		return nil, errors.New("open repository content failed")
	}
	findings, scanErr := scan(reader)
	closeErr := reader.Close()
	if scanErr != nil {
		return nil, scanErr
	}
	if closeErr != nil {
		return nil, errors.New("close repository content failed")
	}
	return findings, nil
}

func findingRuleRank(rule RuleID) int {
	switch rule {
	case RulePrivatePath:
		return 0
	case RuleSecret:
		return 1
	case RuleGitlink:
		return 2
	case RuleBinary:
		return 3
	default:
		return 4
	}
}

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"xagent/internal/repoaudit"
)

const (
	exitPass     = 0
	exitFindings = 1
	exitTool     = 2

	preservationManifestRelative = "preservation/m0.manifest"
)

type commandDeps struct {
	getwd             func() (string, error)
	getenv            func(string) string
	source            func(string, repoaudit.SourceKind, string) repoaudit.Source
	openOwner         func(repoaudit.EvidenceConfig) (*repoaudit.EvidenceOwner, error)
	prepareControl    func(string) error
	indexEntries      func(context.Context, string) ([]repoaudit.Entry, error)
	mutateIndex       func(context.Context, string, []repoaudit.Entry) error
	syncIndex         func(string) error
	checkModules      func(string, []repoaudit.Entry) error
	validateSelection func([]repoaudit.Entry) (string, error)
}

func defaultCommandDeps() commandDeps {
	return commandDeps{
		getwd:  os.Getwd,
		getenv: os.Getenv,
		source: func(repo string, kind repoaudit.SourceKind, revision string) repoaudit.Source {
			switch kind {
			case repoaudit.SourceCommit:
				return repoaudit.NewCommitSource(repo, revision)
			case repoaudit.SourceWorktree:
				return repoaudit.NewWorktreeSource(repo)
			default:
				return repoaudit.NewIndexSource(repo)
			}
		},
		openOwner:      repoaudit.OpenEvidenceOwner,
		prepareControl: repoaudit.EnsureEvidenceControlRoot,
		indexEntries: func(ctx context.Context, repo string) ([]repoaudit.Entry, error) {
			return repoaudit.NewIndexSource(repo).Entries(ctx)
		},
		mutateIndex:       removeIndexEntries,
		syncIndex:         syncRepositoryIndex,
		checkModules:      requireGitmodulesAbsent,
		validateSelection: repoaudit.ValidateApprovedPreservationSelection,
	}
}

func main() {
	code, err := runCommand(context.Background(), os.Args[1:], os.Stdout, defaultCommandDeps())
	if err != nil {
		fmt.Fprintln(os.Stderr, "result=error code=repoaudit_failed")
	}
	os.Exit(code)
}

func runCommand(ctx context.Context, args []string, output io.Writer, deps commandDeps) (int, error) {
	flags := flag.NewFlagSet("xagent-repo-check", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sourceValue := flags.String("source", string(repoaudit.SourceIndex), "")
	revision := flags.String("revision", "", "")
	preservation := flags.String("preservation", "", "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return exitTool, errors.New("invalid command arguments")
	}
	repo, err := deps.getwd()
	if err != nil {
		return exitTool, errors.New("resolve repository root failed")
	}
	repo, err = filepath.Abs(repo)
	if err != nil {
		return exitTool, errors.New("resolve repository root failed")
	}
	if *preservation != "" {
		seenAuditFlag := false
		flags.Visit(func(item *flag.Flag) {
			if item.Name == "source" || item.Name == "revision" {
				seenAuditFlag = true
			}
		})
		if seenAuditFlag {
			return exitTool, errors.New("preservation and audit arguments cannot be combined")
		}
		summary, err := runPreservation(ctx, repo, *preservation, deps)
		if err != nil {
			return exitTool, err
		}
		fmt.Fprintf(output, "action=%s count=%d target_digest=%s manifest_sha256=%s result=pass\n", summary.Action, summary.Count, summary.TargetDigest, summary.ManifestSHA256)
		return exitPass, nil
	}

	kind := repoaudit.SourceKind(*sourceValue)
	if kind != repoaudit.SourceIndex && kind != repoaudit.SourceCommit && kind != repoaudit.SourceWorktree {
		return exitTool, errors.New("unsupported audit source")
	}
	if kind == repoaudit.SourceCommit {
		if !canonicalRevision(*revision) {
			return exitTool, errors.New("commit audit requires a canonical revision")
		}
	} else if *revision != "" {
		return exitTool, errors.New("revision is only valid for commit audit")
	}
	policy, err := loadPolicy(filepath.Join(repo, ".repoaudit.yaml"))
	if err != nil {
		return exitTool, err
	}
	report, err := (repoaudit.Auditor{Policy: policy}).Audit(ctx, deps.source(repo, kind, *revision))
	class := repoaudit.ClassifyAudit(report, err)
	switch class {
	case repoaudit.AuditExitPass:
		fmt.Fprintf(output, "result=pass checked=%d findings=0\n", report.Checked)
		return exitPass, nil
	case repoaudit.AuditExitFindings:
		counts := findingCounts(report.Findings)
		fmt.Fprintf(output, "result=findings checked=%d findings=%d private_path=%d secret=%d gitlink=%d binary=%d\n", report.Checked, len(report.Findings), counts[repoaudit.RulePrivatePath], counts[repoaudit.RuleSecret], counts[repoaudit.RuleGitlink], counts[repoaudit.RuleBinary])
		return exitFindings, nil
	default:
		return exitTool, errors.New("repository audit failed")
	}
}

func findingCounts(findings []repoaudit.Finding) map[repoaudit.RuleID]int {
	counts := make(map[repoaudit.RuleID]int)
	for _, finding := range findings {
		counts[finding.RuleID]++
	}
	return counts
}

func loadPolicy(name string) (repoaudit.Policy, error) {
	file, err := os.Open(name)
	if err != nil {
		return repoaudit.Policy{}, errors.New("open repoaudit policy failed")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, repoaudit.MaxPolicyBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > repoaudit.MaxPolicyBytes {
		return repoaudit.Policy{}, errors.New("read repoaudit policy failed")
	}
	policy, err := repoaudit.ParsePolicy(data)
	if err != nil {
		return repoaudit.Policy{}, errors.New("parse repoaudit policy failed")
	}
	return policy, nil
}

type preservationSummary struct {
	Action         string
	Count          int
	TargetDigest   string
	ManifestSHA256 string
}

func runPreservation(ctx context.Context, repo, action string, deps commandDeps) (summary preservationSummary, runErr error) {
	if action != "freeze" && action != "remove-index" && action != "verify-immediate" && action != "verify-final" {
		return summary, errors.New("unsupported preservation action")
	}
	phase := deps.getenv("XAGENT_PRESERVATION_PHASE")
	if action == "verify-final" {
		if phase != "t5.53a" && phase != "t5.53b" {
			return summary, errors.New("verify-final is not authorized at this call site")
		}
	} else if phase != "t0.10" {
		return summary, errors.New("preservation mutation sequence is not authorized at this call site")
	}
	evidenceDir := deps.getenv("XAGENT_EVIDENCE_DIR")
	taskDir := deps.getenv("XAGENT_TASK_TMP")
	if !filepath.IsAbs(evidenceDir) || !filepath.IsAbs(taskDir) {
		return summary, errors.New("preservation roots must be absolute")
	}
	config := repoaudit.EvidenceConfig{
		ControlRoot: filepath.Dir(evidenceDir),
		EvidenceDir: evidenceDir,
		TaskDir:     taskDir,
		Workspace:   repo,
		SourceRoot:  taskDir,
	}
	if err := deps.prepareControl(config.ControlRoot); err != nil {
		return summary, errors.New("prepare preservation evidence control root failed")
	}
	owner, err := deps.openOwner(config)
	if err != nil {
		return summary, errors.New("open preservation evidence owner failed")
	}
	defer func() {
		if closeErr := owner.Close(); closeErr != nil {
			runErr = errors.Join(runErr, errors.New("close preservation evidence owner failed"))
		}
	}()
	if err := owner.EnsureDir("preservation"); err != nil {
		return summary, errors.New("prepare preservation ledger failed")
	}
	entries, err := deps.indexEntries(ctx, repo)
	if err != nil {
		return summary, errors.New("read preservation index failed")
	}

	if action == "freeze" {
		selected, err := repoaudit.SelectPreservationEntries(entries)
		if err != nil {
			return summary, err
		}
		targetDigest, err := deps.validateSelection(selected)
		if err != nil {
			return summary, err
		}
		if err := deps.checkModules(repo, entries); err != nil {
			return summary, err
		}
		preIndex, err := repoaudit.IndexIdentityDigest(entries)
		if err != nil {
			return summary, err
		}
		manifest, err := repoaudit.BuildPreservationManifest(repo, selected)
		if err != nil {
			return summary, errors.New("build preservation manifest failed")
		}
		manifest.PreIndexSHA256 = preIndex
		manifest.GitmodulesAbsent = true
		data, err := repoaudit.CanonicalPreservationBytes(manifest)
		if err != nil {
			return summary, err
		}
		if err := owner.Publish(preservationManifestRelative, data); err != nil {
			return summary, errors.New("publish preservation manifest failed")
		}
		return newPreservationSummary(action, selected, targetDigest, data), nil
	}

	data, err := owner.Read(preservationManifestRelative)
	if err != nil {
		return summary, errors.New("read preservation manifest failed")
	}
	manifest, err := repoaudit.ParsePreservationBytes(data)
	if err != nil || !manifest.GitmodulesAbsent {
		return summary, errors.New("validate preservation manifest failed")
	}
	targets := repoaudit.ManifestIndexEntries(manifest)
	targetDigest, err := deps.validateSelection(targets)
	if err != nil {
		return summary, err
	}
	if err := deps.checkModules(repo, entries); err != nil {
		return summary, err
	}
	if err := repoaudit.VerifyPreservationManifest(repo, manifest, targets); err != nil {
		return summary, errors.New("verify preservation worktree failed")
	}

	switch action {
	case "remove-index":
		if err := verifyImmediateIndex(entries, manifest, targets); err != nil {
			currentDigest, digestErr := repoaudit.IndexIdentityDigest(entries)
			if digestErr != nil || currentDigest != manifest.PreIndexSHA256 {
				return summary, errors.New("index is neither the frozen pre-state nor the exact post-state")
			}
			selected, selectErr := repoaudit.SelectPreservationEntries(entries)
			if selectErr != nil {
				return summary, selectErr
			}
			if digest, selectionErr := repoaudit.PreservationSelectionDigest(selected); selectionErr != nil || digest != targetDigest {
				return summary, errors.New("current preservation selection differs from manifest")
			}
			if err := deps.mutateIndex(ctx, repo, targets); err != nil {
				return summary, errors.New("remove preservation targets from index failed")
			}
		}
		if err := deps.syncIndex(repo); err != nil {
			return summary, errors.New("sync repository index failed")
		}
		entries, err = deps.indexEntries(ctx, repo)
		if err != nil {
			return summary, errors.New("read post-mutation index failed")
		}
		if err := verifyImmediateIndex(entries, manifest, targets); err != nil {
			return summary, err
		}
	case "verify-immediate":
		if err := verifyImmediateIndex(entries, manifest, targets); err != nil {
			return summary, err
		}
	case "verify-final":
		selected, err := repoaudit.SelectPreservationEntries(entries)
		if err != nil || len(selected) != 0 {
			return summary, errors.New("preservation target reappeared in index")
		}
	}
	if err := deps.checkModules(repo, entries); err != nil {
		return summary, err
	}
	if err := repoaudit.VerifyPreservationManifest(repo, manifest, targets); err != nil {
		return summary, errors.New("post-action preservation verification failed")
	}
	return newPreservationSummary(action, targets, targetDigest, data), nil
}

func newPreservationSummary(action string, targets []repoaudit.Entry, digest string, data []byte) preservationSummary {
	manifestDigest := sha256.Sum256(data)
	return preservationSummary{Action: action, Count: len(targets), TargetDigest: digest, ManifestSHA256: hex.EncodeToString(manifestDigest[:])}
}

func verifyImmediateIndex(current []repoaudit.Entry, manifest repoaudit.PreservationManifest, targets []repoaudit.Entry) error {
	selected, err := repoaudit.SelectPreservationEntries(current)
	if err != nil || len(selected) != 0 {
		return errors.New("preservation targets remain in index")
	}
	// The complete expected post-index identity is derived by requiring that
	// adding the frozen target entries back recreates the frozen pre-index.
	reconstructed := append(append([]repoaudit.Entry(nil), current...), targets...)
	digest, err := repoaudit.IndexIdentityDigest(reconstructed)
	if err != nil || digest != manifest.PreIndexSHA256 {
		return errors.New("post-index differs from frozen index minus preservation targets")
	}
	return nil
}

func requireGitmodulesAbsent(repo string, entries []repoaudit.Entry) error {
	for _, entry := range entries {
		if entry.Path == ".gitmodules" {
			return errors.New(".gitmodules is present in index")
		}
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return errors.New("open repository root failed")
	}
	defer root.Close()
	_, err = root.Lstat(".gitmodules")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("inspect .gitmodules sentinel failed")
	}
	return errors.New(".gitmodules is present in worktree")
}

func removeIndexEntries(ctx context.Context, repo string, entries []repoaudit.Entry) error {
	var input bytes.Buffer
	for _, entry := range entries {
		input.WriteString(entry.Path)
		input.WriteByte(0)
	}
	command := exec.CommandContext(ctx, "git", "-C", repo, "--no-pager", "--no-replace-objects", "update-index", "--force-remove", "-z", "--stdin")
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1")
	command.Stdin = &input
	if err := command.Run(); err != nil {
		return errors.New("git update-index failed")
	}
	return nil
}

func syncRepositoryIndex(repo string) error {
	indexPath := filepath.Join(repo, ".git", "index")
	file, err := os.Open(indexPath)
	if err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(indexPath))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func canonicalRevision(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

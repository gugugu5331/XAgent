package repoaudit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestAuditOrderingAndExitClass(t *testing.T) {
	t.Parallel()

	policy := Policy{Version: PolicyVersion, MaxBinaryBytes: MaxBinaryBytes}
	source := &memoryAuditSource{
		entries: []Entry{
			{Path: "z.bin", Mode: 0100644, Type: "blob"},
			{Path: ".mewcode/memory/private.txt", Mode: 0100644, Type: "blob"},
			{Path: "a.txt", Mode: 0100644, Type: "blob"},
		},
		content: map[string][]byte{
			"z.bin":                       {0},
			".mewcode/memory/private.txt": []byte("ordinary\n"),
			"a.txt":                       []byte("ordinary\nOPENAI_API_KEY=audit-canary-12345678\n"),
		},
	}
	report, err := (Auditor{Policy: policy}).Audit(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if report.Checked != 3 || ClassifyAudit(report, nil) != AuditExitFindings {
		t.Fatalf("audit report/class = %#v/%v", report, ClassifyAudit(report, nil))
	}
	got := make([]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		got = append(got, finding.Path+"\x00"+string(finding.RuleID))
	}
	want := []string{
		".mewcode/memory/private.txt\x00private-path",
		"a.txt\x00secret",
		"z.bin\x00binary",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("finding order = %#v, want %#v", got, want)
	}

	pass, err := (Auditor{Policy: policy}).Audit(context.Background(), &memoryAuditSource{
		entries: []Entry{{Path: "safe.txt", Mode: 0100644, Type: "blob"}},
		content: map[string][]byte{"safe.txt": []byte("ordinary\n")},
	})
	if err != nil || ClassifyAudit(pass, err) != AuditExitPass {
		t.Fatalf("pass audit = %#v, err=%v, class=%v", pass, err, ClassifyAudit(pass, err))
	}

	toolErr := errors.New("source failure includes sensitive material")
	failed, err := (Auditor{Policy: policy}).Audit(context.Background(), &memoryAuditSource{entriesErr: toolErr})
	if err == nil || ClassifyAudit(failed, err) != AuditExitError || errors.Is(err, toolErr) {
		t.Fatalf("tool-error audit = %#v, err=%v, class=%v", failed, err, ClassifyAudit(failed, err))
	}
}

type memoryAuditSource struct {
	entries    []Entry
	content    map[string][]byte
	entriesErr error
}

func (s *memoryAuditSource) Entries(context.Context) ([]Entry, error) {
	if s.entriesErr != nil {
		return nil, s.entriesErr
	}
	return append([]Entry(nil), s.entries...), nil
}

func (s *memoryAuditSource) Open(_ context.Context, entry Entry) (io.ReadCloser, error) {
	content, ok := s.content[entry.Path]
	if !ok {
		return nil, errors.New("missing memory content")
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

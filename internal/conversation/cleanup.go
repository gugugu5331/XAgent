package conversation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type CleanupReport struct {
	Deleted     int               `json:"deleted"`
	Skipped     int               `json:"skipped"`
	Diagnostics []JSONLDiagnostic `json:"diagnostics,omitempty"`
}

func (s *JSONLStore) Cleanup(ctx context.Context, retention time.Duration) (CleanupReport, error) {
	if err := ctxErr(ctx); err != nil {
		return CleanupReport{}, err
	}
	if retention <= 0 {
		return CleanupReport{}, fmt.Errorf("会话保留时间必须大于 0")
	}
	realRoot, err := filepath.EvalSymlinks(s.dataDir)
	if err != nil {
		return CleanupReport{}, fmt.Errorf("解析会话目录失败: %w", err)
	}
	realRoot, err = filepath.Abs(realRoot)
	if err != nil {
		return CleanupReport{}, fmt.Errorf("解析会话目录绝对路径失败: %w", err)
	}
	realRoot = filepath.Clean(realRoot)

	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return CleanupReport{}, fmt.Errorf("读取会话目录失败: %w", err)
	}
	cutoff := s.now().Add(-retention)
	report := CleanupReport{}
	for _, entry := range entries {
		if err := ctxErr(ctx); err != nil {
			return report, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(s.dataDir, entry.Name())
		realPath, err := filepath.EvalSymlinks(path)
		if err != nil {
			report.Skipped++
			report.Diagnostics = append(report.Diagnostics, newJSONLDiagnostic("jsonl_cleanup_resolve_failed", fmt.Sprintf("无法解析会话文件真实路径: %v", err), JSONLSeverityWarning).WithPath(path))
			continue
		}
		realPath, err = filepath.Abs(realPath)
		if err != nil {
			report.Skipped++
			report.Diagnostics = append(report.Diagnostics, newJSONLDiagnostic("jsonl_cleanup_resolve_failed", fmt.Sprintf("无法解析会话文件绝对路径: %v", err), JSONLSeverityWarning).WithPath(path))
			continue
		}
		realPath = filepath.Clean(realPath)
		if !withinCleanupRoot(realPath, realRoot) {
			report.Skipped++
			report.Diagnostics = append(report.Diagnostics, newJSONLDiagnostic("jsonl_cleanup_path_escape", "会话文件真实路径跳出数据目录，已跳过", JSONLSeverityWarning).WithPath(realPath))
			continue
		}
		conversation, _, err := s.loadFromPath(ctx, path, strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
		if err != nil {
			report.Skipped++
			continue
		}
		if conversation.UpdatedAt.IsZero() || !conversation.UpdatedAt.Before(cutoff) {
			continue
		}
		if err := os.Remove(realPath); err != nil {
			report.Skipped++
			report.Diagnostics = append(report.Diagnostics, newJSONLDiagnostic("jsonl_cleanup_delete_failed", fmt.Sprintf("删除过期会话失败: %v", err), JSONLSeverityWarning).WithPath(realPath))
			continue
		}
		report.Deleted++
	}
	return report, nil
}

func withinCleanupRoot(path string, root string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

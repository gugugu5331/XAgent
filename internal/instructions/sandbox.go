package instructions

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"xagent/internal/diagnostics"
)

func readInstructionFile(path string, allowedRoot string, maxBytes int64, reportMissing bool, sourceName string) ([]byte, string, bool, *diagnostics.Diagnostic) {
	requested := filepath.Clean(path)
	actualPath, ok, diag := resolveAllowedPath(requested, allowedRoot, sourceName)
	if diag != nil {
		return nil, "", false, diag
	}
	if !ok {
		return nil, "", false, nil
	}

	info, err := os.Stat(actualPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if reportMissing {
				diag := newDiagnostic("instructions_include_missing", "@include 文件不存在，已跳过", sourceName, requested)
				return nil, "", false, &diag
			}
			return nil, "", false, nil
		}
		diag := newDiagnostic("instructions_stat_failed", fmt.Sprintf("无法读取指令文件信息：%v", err), sourceName, requested)
		return nil, "", false, &diag
	}
	if info.IsDir() {
		diag := newDiagnostic("instructions_path_is_directory", "指令路径是目录，已跳过", sourceName, actualPath)
		return nil, "", false, &diag
	}
	if maxBytes > 0 && info.Size() > maxBytes {
		diag := newDiagnostic("instructions_file_too_large", fmt.Sprintf("指令文件超过大小限制：%d > %d", info.Size(), maxBytes), sourceName, actualPath)
		return nil, "", false, &diag
	}

	file, err := os.Open(actualPath)
	if err != nil {
		diag := newDiagnostic("instructions_read_failed", fmt.Sprintf("无法读取指令文件：%v", err), sourceName, actualPath)
		return nil, "", false, &diag
	}
	defer file.Close()

	openedInfo, err := file.Stat()
	if err != nil {
		diag := newDiagnostic("instructions_stat_failed", fmt.Sprintf("无法读取已打开指令文件信息：%v", err), sourceName, actualPath)
		return nil, "", false, &diag
	}
	if openedInfo.IsDir() {
		diag := newDiagnostic("instructions_path_is_directory", "已打开指令路径是目录，已跳过", sourceName, actualPath)
		return nil, "", false, &diag
	}
	if maxBytes > 0 && openedInfo.Size() > maxBytes {
		diag := newDiagnostic("instructions_file_too_large", fmt.Sprintf("已打开指令文件超过大小限制：%d > %d", openedInfo.Size(), maxBytes), sourceName, actualPath)
		return nil, "", false, &diag
	}

	postOpenPath, ok, diag := resolveAllowedPath(actualPath, allowedRoot, sourceName)
	if diag != nil {
		return nil, "", false, diag
	}
	if !ok || postOpenPath != actualPath {
		diag := newDiagnostic("instructions_path_changed", "指令文件打开前后真实路径不一致，已拒绝", sourceName, actualPath)
		return nil, "", false, &diag
	}

	limit := maxBytes
	if limit <= 0 {
		limit = 64 * 1024
	}
	data, err := io.ReadAll(file)
	if err != nil {
		diag := newDiagnostic("instructions_read_failed", fmt.Sprintf("无法读取指令文件：%v", err), sourceName, actualPath)
		return nil, "", false, &diag
	}
	if int64(len(data)) > limit {
		diag := newDiagnostic("instructions_file_too_large", fmt.Sprintf("指令文件超过读取限制：%d > %d", len(data), limit), sourceName, actualPath)
		return nil, "", false, &diag
	}
	return data, actualPath, true, nil
}

func resolveAllowedPath(path string, allowedRoot string, sourceName string) (string, bool, *diagnostics.Diagnostic) {
	root := strings.TrimSpace(allowedRoot)
	if root == "" {
		diag := newDiagnostic("instructions_empty_root", "指令允许根目录为空，已跳过", sourceName, path)
		return "", false, &diag
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		diag := newDiagnostic("instructions_root_resolve_failed", fmt.Sprintf("无法解析指令根目录：%v", err), sourceName, root)
		return "", false, &diag
	}
	realRoot, err = filepath.Abs(realRoot)
	if err != nil {
		diag := newDiagnostic("instructions_root_resolve_failed", fmt.Sprintf("无法解析指令根绝对路径：%v", err), sourceName, root)
		return "", false, &diag
	}

	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		abs, err := filepath.Abs(cleaned)
		if err != nil {
			diag := newDiagnostic("instructions_path_resolve_failed", fmt.Sprintf("无法解析指令绝对路径：%v", err), sourceName, path)
			return "", false, &diag
		}
		cleaned = abs
	}
	realPath, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", true, nil
		}
		diag := newDiagnostic("instructions_path_resolve_failed", fmt.Sprintf("无法解析指令真实路径：%v", err), sourceName, cleaned)
		return "", false, &diag
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil {
		diag := newDiagnostic("instructions_path_resolve_failed", fmt.Sprintf("无法解析指令绝对路径：%v", err), sourceName, realPath)
		return "", false, &diag
	}
	if !withinRoot(realPath, realRoot) {
		diag := newDiagnostic("instructions_path_escape", "指令路径跳出允许根目录，已拒绝", sourceName, realPath)
		return "", false, &diag
	}
	return filepath.Clean(realPath), true, nil
}

func withinRoot(path string, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

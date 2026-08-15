package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"xagent/internal/redact"
)

type Writer struct {
	mu           sync.Mutex
	redactor     *redact.RuntimeRedactor
	maxNoteBytes int
}

func (w *Writer) WriteNote(root string, note Note) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	note, err = sanitizeBoundedNote(note, w.redactor, w.maxNoteBytes)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("创建记忆目录失败: %w", err)
	}
	return atomicWriteFile(filepath.Join(root, noteFileName(note.ID)), []byte(MarshalNote(note)), 0o600)
}

func (w *Writer) WriteIndex(root string, index Index, maxLines int, maxBytes int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	redactor := w.redactor
	if redactor == nil {
		redactor = redact.NewRuntimeRedactor()
	}
	for entryIndex := range index.Entries {
		entry := &index.Entries[entryIndex]
		entry.ID = safeID(entry.ID)
		entry.Title = redactor.Text(entry.Title)
		entry.Body = redactor.Text(entry.Body)
		entry.Path = noteFileName(entry.ID)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("创建记忆目录失败: %w", err)
	}
	return atomicWriteFile(filepath.Join(root, IndexFileName), []byte(MarshalIndex(index, maxLines, maxBytes)), 0o600)
}

func (w *Writer) DeleteNote(root string, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	path, err := safeNotePath(root, id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除记忆失败: %w", err)
	}
	return nil
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func safeNotePath(root string, id string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("解析记忆目录失败: %w", err)
	}
	realRoot, err = filepath.Abs(realRoot)
	if err != nil {
		return "", fmt.Errorf("解析记忆目录绝对路径失败: %w", err)
	}
	path := filepath.Join(realRoot, noteFileName(id))
	realPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析记忆路径失败: %w", err)
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == "../" {
		return "", fmt.Errorf("记忆路径跳出允许目录")
	}
	return realPath, nil
}

func noteFileName(id string) string {
	return safeID(id) + ".md"
}

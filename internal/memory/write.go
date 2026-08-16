package memory

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"xagent/internal/redact"
)

type Writer struct {
	mu                   sync.Mutex
	redactor             *redact.RuntimeRedactor
	maxNoteBytes         int
	workspaceTempCreated func()
}

func (w *Writer) WriteNoteAt(root *os.Root, note Note) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	note, err = sanitizeBoundedNote(note, w.redactor, w.maxNoteBytes)
	if err != nil {
		return err
	}
	return atomicWriteRootWithBarrier(root, noteFileName(note.ID), []byte(MarshalNote(note)), 0o600, w.workspaceTempCreated)
}

func (w *Writer) WriteIndexAt(root *os.Root, index Index, maxLines int, maxBytes int) error {
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
	return atomicWriteRootWithBarrier(root, IndexFileName, []byte(MarshalIndex(index, maxLines, maxBytes)), 0o600, w.workspaceTempCreated)
}

func (w *Writer) DeleteNoteAt(root *os.Root, id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if root == nil || safeID(id) == "" || safeID(id) != id {
		return fmt.Errorf("记忆路径无效")
	}
	if err := root.Remove(noteFileName(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除记忆失败")
	}
	return nil
}

func atomicWriteRoot(root *os.Root, path string, data []byte, perm os.FileMode) error {
	return atomicWriteRootWithBarrier(root, path, data, perm, nil)
}

func atomicWriteRootWithBarrier(root *os.Root, path string, data []byte, perm os.FileMode, tempCreated func()) error {
	if root == nil || !safeRootBaseName(path) || perm != perm.Perm() {
		return fmt.Errorf("workspace memory root is unavailable")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("创建记忆临时文件失败")
	}
	temporary := ".tmp-" + hex.EncodeToString(random[:])
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("创建记忆临时文件失败")
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = root.Remove(temporary)
		}
	}()
	if tempCreated != nil {
		tempCreated()
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入记忆临时文件失败")
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return fmt.Errorf("设置记忆文件权限失败")
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭记忆临时文件失败")
	}
	if err := root.Rename(temporary, path); err != nil {
		return fmt.Errorf("发布记忆文件失败")
	}
	removeTemporary = false
	return nil
}

func safeRootBaseName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name &&
		!strings.ContainsAny(name, `/\\`)
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

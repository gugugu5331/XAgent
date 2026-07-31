package artifact

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

type writerState uint8

const (
	writerActive writerState = iota + 1
	writerCommitted
	writerAborted
)

var errWriterFinalized = errors.New("artifact writer is already finalized")

type fileWriter struct {
	mu sync.Mutex

	store       *fileStore
	file        *os.File
	id          string
	stagingPath string
	finalPath   string
	metadata    Metadata
	createdAt   time.Time
	bytes       int64
	incomplete  bool
	state       writerState
}

func (w *fileWriter) Write(data []byte) (int, error) {
	if w == nil {
		return 0, errors.New("artifact writer is unavailable")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != writerActive || w.file == nil || w.store == nil {
		return 0, errWriterFinalized
	}
	if len(data) == 0 {
		return 0, nil
	}
	allowed, limitErr := w.store.reserve(w, int64(len(data)))
	if allowed == 0 {
		w.incomplete = true
		return 0, limitErr
	}
	written, writeErr := w.file.Write(data[:int(allowed)])
	if int64(written) < allowed {
		w.store.releaseReserved(allowed - int64(written))
	}
	w.bytes += int64(written)
	if writeErr != nil || int64(written) != allowed {
		w.incomplete = true
		return written, errors.New("artifact staging write failed")
	}
	if limitErr != nil {
		w.incomplete = true
		return written, limitErr
	}
	return written, nil
}

func (w *fileWriter) Commit(ctx context.Context) (Ref, error) {
	if w == nil || ctx == nil {
		return Ref{}, errors.New("artifact commit request is invalid")
	}
	select {
	case <-ctx.Done():
		return Ref{}, errors.New("artifact commit canceled")
	default:
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != writerActive || w.file == nil || w.store == nil {
		return Ref{}, errWriterFinalized
	}
	w.state = writerCommitted
	ref := Ref{
		ID:        w.id,
		Bytes:     w.bytes,
		CreatedAt: w.createdAt,
		Available: false,
		Complete:  !w.incomplete,
	}
	if err := w.file.Sync(); err != nil {
		w.failCommit()
		return ref, errors.New("artifact commit failed")
	}
	if err := w.file.Close(); err != nil {
		w.file = nil
		w.failCommit()
		return ref, errors.New("artifact commit failed")
	}
	w.file = nil
	if err := os.Rename(w.stagingPath, w.finalPath); err != nil {
		w.failCommit()
		return ref, errors.New("artifact commit failed")
	}
	ref.Available = true
	if err := w.store.commitWriter(w, ref); err != nil {
		_ = os.Remove(w.finalPath)
		w.store.abortWriter(w.id, w.bytes)
		return Ref{ID: ref.ID, Bytes: ref.Bytes, CreatedAt: ref.CreatedAt, Complete: false}, errors.New("artifact commit failed")
	}
	w.stagingPath = ""
	w.finalPath = ""
	return ref, nil
}

func (w *fileWriter) failCommit() {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	_ = os.Remove(w.stagingPath)
	w.store.abortWriter(w.id, w.bytes)
	w.stagingPath = ""
	w.finalPath = ""
}

func (w *fileWriter) Abort() error {
	if w == nil {
		return errors.New("artifact writer is unavailable")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != writerActive || w.file == nil || w.store == nil {
		return errWriterFinalized
	}
	w.state = writerAborted
	closeErr := w.file.Close()
	w.file = nil
	removeErr := os.Remove(w.stagingPath)
	w.store.abortWriter(w.id, w.bytes)
	w.stagingPath = ""
	w.finalPath = ""
	if closeErr != nil || (removeErr != nil && !errors.Is(removeErr, os.ErrNotExist)) {
		return errors.New("artifact abort failed")
	}
	return nil
}

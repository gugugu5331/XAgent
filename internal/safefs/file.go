package safefs

import (
	"errors"
	"io"
	"os"
	"sync"
)

type platformOpenedFile struct {
	file     *os.File
	identity objectIdentity
}

// File is a pathless read handle opened beneath a Root.
type File struct {
	mu sync.Mutex

	handle   *os.File
	identity objectIdentity
	closed   bool
	closeErr error
}

func newFile(opened platformOpenedFile) *File {
	return &File{handle: opened.file, identity: opened.identity}
}

func (f *File) Read(buffer []byte) (int, error) {
	if f == nil {
		return 0, errors.New("safefs file is unavailable")
	}
	f.mu.Lock()
	if f.closed || f.handle == nil {
		f.mu.Unlock()
		return 0, errors.New("safefs file is closed")
	}
	handle := f.handle
	f.mu.Unlock()

	count, err := handle.Read(buffer)
	if err == nil || errors.Is(err, io.EOF) {
		return count, err
	}
	return count, errors.New("safefs file read failed")
}

// Close is idempotent and returns the same final result to every caller.
func (f *File) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return f.closeErr
	}
	f.closed = true
	if f.handle != nil {
		if err := f.handle.Close(); err != nil {
			f.closeErr = errors.New("safefs file close failed")
		}
		f.handle = nil
	}
	return f.closeErr
}

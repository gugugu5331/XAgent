package safefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"

	"xagent/internal/budget"
)

func (r *Root) Walk(ctx context.Context, relative string, counter *budget.Counter, visit func(Entry) error) error {
	if ctx == nil || counter == nil || visit == nil {
		return errors.New("safefs walk request is invalid")
	}
	base, err := canonicalWalkRoot(relative)
	if err != nil {
		return err
	}
	if err := checkWalkContext(ctx); err != nil {
		return err
	}
	if err := counter.Consume(budget.Directories, 1); err != nil {
		return err
	}
	pending := []string{base}
	for len(pending) != 0 {
		index := len(pending) - 1
		directoryPath := pending[index]
		pending = pending[:index]
		if err := checkWalkContext(ctx); err != nil {
			return err
		}
		directory, err := r.openWalkDirectory(directoryPath)
		if err != nil {
			return err
		}
		for {
			if err := checkWalkContext(ctx); err != nil {
				_ = directory.Close()
				return err
			}
			entries, readErr := directory.ReadDir(1)
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil || len(entries) != 1 {
				_ = directory.Close()
				return errors.New("safefs walk directory read failed")
			}
			entry := entries[0]
			name := entry.Name()
			if !validWalkEntryName(name) {
				_ = directory.Close()
				return errors.New("safefs walk entry is invalid")
			}
			if err := checkWalkContext(ctx); err != nil {
				_ = directory.Close()
				return err
			}
			info, infoErr := platformDirectoryEntryInfo(directory, name)
			if infoErr != nil {
				_ = directory.Close()
				return errors.New("safefs walk entry information failed")
			}
			child := name
			if directoryPath != "." {
				child = directoryPath + "/" + name
			}
			dimension := budget.Files
			if info.mode.IsDir() {
				dimension = budget.Directories
			}
			if err := counter.Consume(dimension, 1); err != nil {
				_ = directory.Close()
				return err
			}
			if err := checkWalkContext(ctx); err != nil {
				_ = directory.Close()
				return err
			}
			observed := Entry{Path: child, Name: name, Mode: info.mode, Size: info.size}
			if err := visit(observed); err != nil {
				_ = directory.Close()
				return err
			}
			if observed.IsDir() {
				pending = append(pending, child)
			}
		}
		if err := directory.Close(); err != nil {
			return errors.New("safefs walk directory close failed")
		}
	}
	return nil
}

type directoryEntryInfo struct {
	mode fs.FileMode
	size int64
}

func (r *Root) openWalkDirectory(relative string) (*os.File, error) {
	if r == nil {
		return nil, errors.New("safefs root is unavailable")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || r.backend == nil {
		return nil, errors.New("safefs root is closed")
	}
	directory, err := r.backend.openDirectory(relative)
	if err != nil || directory == nil {
		if directory != nil {
			_ = directory.Close()
		}
		return nil, errors.New("safefs walk directory open failed")
	}
	return directory, nil
}

func canonicalWalkRoot(relative string) (string, error) {
	if relative == "." {
		return relative, nil
	}
	return canonicalRelative(relative)
}

func checkWalkContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func validWalkEntryName(name string) bool {
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) && !strings.ContainsAny(name, "/\\\x00")
}

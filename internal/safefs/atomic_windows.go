//go:build windows

package safefs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsAtomicStageAttempts = 16

type windowsFileRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

func (r *windowsRoot) atomicWrite(
	ctx context.Context,
	relative string,
	_ fs.FileMode,
	write func(io.Writer) error,
	validate func(bindingResolution) error,
) error {
	parent, owned, leaf, err := r.openAtomicParent(relative)
	if err != nil {
		return errors.New("safefs atomic parent open failed")
	}
	if owned {
		defer windows.CloseHandle(parent)
	}
	stage, _, err := createWindowsAtomicStage(parent)
	if err != nil {
		return errors.New("safefs atomic staging failed")
	}
	stageOpen := true
	stagePublished := false
	defer func() {
		if stageOpen {
			if !stagePublished {
				_ = markWindowsFileForDeletion(windows.Handle(stage.Fd()))
			}
			_ = stage.Close()
		}
	}()

	select {
	case <-ctx.Done():
		return errors.New("safefs atomic write canceled")
	default:
	}
	if err := write(struct{ io.Writer }{Writer: stage}); err != nil {
		return errors.New("safefs atomic writer failed")
	}
	if err := stage.Sync(); err != nil {
		return errors.New("safefs atomic staging sync failed")
	}
	select {
	case <-ctx.Done():
		return errors.New("safefs atomic write canceled")
	default:
	}
	live, err := r.bind(relative)
	if err != nil || validate(live) != nil {
		return errors.New("safefs atomic revalidation failed")
	}
	if err := renameWindowsStage(windows.Handle(stage.Fd()), parent, leaf); err != nil {
		return errors.New("safefs atomic replace failed")
	}
	stagePublished = true
	if err := stage.Sync(); err != nil {
		return errors.New("safefs atomic publish sync failed")
	}
	if err := stage.Close(); err != nil {
		stageOpen = false
		return errors.New("safefs atomic staging close failed")
	}
	stageOpen = false
	return nil
}

func (r *windowsRoot) openAtomicParent(relative string) (windows.Handle, bool, string, error) {
	if r == nil || r.handle == 0 || r.handle == windows.InvalidHandle {
		return windows.InvalidHandle, false, "", errors.New("safefs platform root is closed")
	}
	components := strings.Split(relative, "/")
	leaf := components[len(components)-1]
	current := r.handle
	owned := false
	for _, component := range components[:len(components)-1] {
		next, err := windowsOpenRelative(current, component, true)
		if err != nil {
			if owned {
				_ = windows.CloseHandle(current)
			}
			return windows.InvalidHandle, false, "", errors.New("safefs platform parent open failed")
		}
		if owned {
			_ = windows.CloseHandle(current)
		}
		current = next
		owned = true
	}
	return current, owned, leaf, nil
}

func createWindowsAtomicStage(parent windows.Handle) (*os.File, string, error) {
	for attempt := 0; attempt < windowsAtomicStageAttempts; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := ".xagent-stage-" + hex.EncodeToString(random[:])
		objectName, err := windows.NewNTUnicodeString(name)
		if err != nil {
			return nil, "", err
		}
		attributes := windows.OBJECT_ATTRIBUTES{
			Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
			RootDirectory: parent,
			ObjectName:    objectName,
			Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		}
		var handle windows.Handle
		var status windows.IO_STATUS_BLOCK
		var allocation int64
		err = windows.NtCreateFile(
			&handle,
			windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.DELETE|windows.SYNCHRONIZE,
			&attributes,
			&status,
			&allocation,
			0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
			0,
			0,
		)
		if err == nil {
			file := os.NewFile(uintptr(handle), "")
			if file == nil {
				_ = windows.CloseHandle(handle)
				return nil, "", errors.New("safefs atomic staging conversion failed")
			}
			return file, name, nil
		}
		if err != windows.STATUS_OBJECT_NAME_COLLISION {
			return nil, "", err
		}
	}
	return nil, "", errors.New("safefs atomic staging collision limit reached")
}

func renameWindowsStage(stage, parent windows.Handle, leaf string) error {
	encoded, err := windows.UTF16FromString(leaf)
	if err != nil || len(encoded) < 2 {
		return errors.New("safefs atomic target name failed")
	}
	nameBytes := (len(encoded) - 1) * 2
	var layout windowsFileRenameInformation
	bufferSize := int(unsafe.Offsetof(layout.FileName)) + nameBytes
	buffer := make([]byte, bufferSize)
	information := (*windowsFileRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.ReplaceIfExists = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	information.RootDirectory = parent
	information.FileNameLength = uint32(nameBytes)
	destination := unsafe.Slice(&information.FileName[0], len(encoded)-1)
	copy(destination, encoded[:len(encoded)-1])
	var status windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(stage, &status, &buffer[0], uint32(bufferSize), windows.FileRenameInformation)
}

func markWindowsFileForDeletion(handle windows.Handle) error {
	deleteFile := byte(1)
	return windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &deleteFile, 1)
}

//go:build windows

package safefs

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsRoot struct {
	handle windows.Handle
	rootID objectIdentity
}

func openPlatformRoot(rootPath string) (rootBackend, error) {
	name, err := windows.UTF16PtrFromString(rootPath)
	if err != nil {
		return nil, errors.New("safefs platform root name failed")
	}
	handle, err := windows.CreateFile(
		name,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, errors.New("safefs platform root open failed")
	}
	identity, directory, err := windowsHandleIdentity(handle)
	if err != nil || !directory {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("safefs platform root identity failed")
	}
	return &windowsRoot{handle: handle, rootID: identity}, nil
}

func (r *windowsRoot) identity() objectIdentity {
	if r == nil {
		return objectIdentity{}
	}
	return r.rootID
}

func (r *windowsRoot) bind(relative string) (bindingResolution, error) {
	if r == nil || r.handle == 0 || r.handle == windows.InvalidHandle {
		return bindingResolution{}, errors.New("safefs platform root is closed")
	}
	components := strings.Split(relative, "/")
	leaf := components[len(components)-1]
	current := r.handle
	owned := false
	defer func() {
		if owned {
			_ = windows.CloseHandle(current)
		}
	}()

	for _, component := range components[:len(components)-1] {
		next, err := windowsOpenRelative(current, component, true)
		if err != nil {
			return bindingResolution{}, errors.New("safefs platform parent open failed")
		}
		if owned {
			_ = windows.CloseHandle(current)
		}
		current = next
		owned = true
	}

	parent, directory, err := windowsHandleIdentity(current)
	if err != nil || !directory {
		return bindingResolution{}, errors.New("safefs platform parent identity failed")
	}
	canonicalLeaf := strings.ToUpper(leaf)
	target, err := windowsOpenRelative(current, leaf, false)
	if err != nil {
		if windowsTargetMissing(err) {
			return bindingResolution{kind: missingBinding, parent: parent, leaf: canonicalLeaf}, nil
		}
		return bindingResolution{}, errors.New("safefs platform target open failed")
	}
	defer windows.CloseHandle(target)
	object, _, err := windowsHandleIdentity(target)
	if err != nil {
		return bindingResolution{}, errors.New("safefs platform target identity failed")
	}
	return bindingResolution{
		kind:   existingBinding,
		object: object,
		parent: parent,
		leaf:   canonicalLeaf,
	}, nil
}

func (r *windowsRoot) openRead(relative string) (platformOpenedFile, error) {
	if r == nil || r.handle == 0 || r.handle == windows.InvalidHandle {
		return platformOpenedFile{}, errors.New("safefs platform root is closed")
	}
	components := strings.Split(relative, "/")
	leaf := components[len(components)-1]
	current := r.handle
	owned := false
	defer func() {
		if owned {
			_ = windows.CloseHandle(current)
		}
	}()
	for _, component := range components[:len(components)-1] {
		next, err := windowsOpenRelative(current, component, true)
		if err != nil {
			return platformOpenedFile{}, errors.New("safefs platform parent open failed")
		}
		if owned {
			_ = windows.CloseHandle(current)
		}
		current = next
		owned = true
	}
	target, err := windowsOpenRelative(current, leaf, false)
	if err != nil {
		return platformOpenedFile{}, errors.New("safefs platform target open failed")
	}
	identity, directory, err := windowsHandleIdentity(target)
	if err != nil || directory {
		_ = windows.CloseHandle(target)
		return platformOpenedFile{}, errors.New("safefs platform target type rejected")
	}
	file := os.NewFile(uintptr(target), "")
	if file == nil {
		_ = windows.CloseHandle(target)
		return platformOpenedFile{}, errors.New("safefs platform file conversion failed")
	}
	return platformOpenedFile{file: file, identity: identity}, nil
}

func (r *windowsRoot) openDirectory(relative string) (*os.File, error) {
	if r == nil || r.handle == 0 || r.handle == windows.InvalidHandle {
		return nil, errors.New("safefs platform root is closed")
	}
	current := r.handle
	owned := false
	if relative == "." {
		process := windows.CurrentProcess()
		var duplicate windows.Handle
		if err := windows.DuplicateHandle(process, current, process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return nil, errors.New("safefs platform directory open failed")
		}
		current = duplicate
		owned = true
	} else {
		for _, component := range strings.Split(relative, "/") {
			next, err := windowsOpenRelative(current, component, true)
			if err != nil {
				if owned {
					_ = windows.CloseHandle(current)
				}
				return nil, errors.New("safefs platform directory open failed")
			}
			if owned {
				_ = windows.CloseHandle(current)
			}
			current = next
			owned = true
		}
	}
	file := os.NewFile(uintptr(current), "")
	if file == nil {
		_ = windows.CloseHandle(current)
		return nil, errors.New("safefs platform directory conversion failed")
	}
	return file, nil
}

func (r *windowsRoot) leafRelation(first, second string) leafRelation {
	if strings.EqualFold(first, second) {
		return leafEquivalent
	}
	return leafDistinct
}

func (r *windowsRoot) close() error {
	if r == nil || r.handle == 0 || r.handle == windows.InvalidHandle {
		return nil
	}
	err := windows.CloseHandle(r.handle)
	r.handle = windows.InvalidHandle
	return err
}

func windowsOpenRelative(parent windows.Handle, name string, directory bool) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_FOR_BACKUP_INTENT)
	access := uint32(windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
		access |= windows.FILE_LIST_DIRECTORY
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	var allocation int64
	err = windows.NtCreateFile(
		&handle,
		access,
		&attributes,
		&status,
		&allocation,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		options,
		0,
		0,
	)
	if err != nil {
		return windows.InvalidHandle, err
	}
	_, openedDirectory, infoErr := windowsHandleIdentity(handle)
	if infoErr != nil || (directory && !openedDirectory) {
		_ = windows.CloseHandle(handle)
		return windows.InvalidHandle, errors.New("safefs platform reparse or type check failed")
	}
	return handle, nil
}

func windowsHandleIdentity(handle windows.Handle) (objectIdentity, bool, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return objectIdentity{}, false, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return objectIdentity{}, false, errors.New("safefs platform reparse point rejected")
	}
	object := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	identity := objectIdentityFromNumbers(uint64(info.VolumeSerialNumber), object)
	directory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	return identity, directory, nil
}

func windowsTargetMissing(err error) bool {
	return err == windows.STATUS_OBJECT_NAME_NOT_FOUND ||
		err == windows.STATUS_OBJECT_PATH_NOT_FOUND ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func platformDirectoryEntryInfo(directory *os.File, name string) (directoryEntryInfo, error) {
	if directory == nil || name == "" {
		return directoryEntryInfo{}, errors.New("safefs platform directory entry is invalid")
	}
	handle, err := windowsOpenRelative(windows.Handle(directory.Fd()), name, false)
	if err != nil {
		return directoryEntryInfo{}, err
	}
	defer windows.CloseHandle(handle)
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return directoryEntryInfo{}, err
	}
	mode := fs.FileMode(0)
	if information.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		mode |= fs.ModeDir
	}
	size := int64(uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow))
	return directoryEntryInfo{mode: mode, size: size}, nil
}

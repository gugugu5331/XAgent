//go:build windows

package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsPrivateRootAccess = windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.DELETE | windows.READ_CONTROL

type windowsFileIdentity struct {
	volume uint32
	high   uint32
	low    uint32
}

type windowsPrivateArtifactRoot struct {
	mu     sync.RWMutex
	handle windows.Handle
}

type windowsDirectoryWalk struct {
	handle     windows.Handle
	identities []windowsFileIdentity
	missing    []string
}

// openPrivateArtifactRoot walks both absolute paths one component at a time.
// Every child is opened relative to an already-open directory handle, and a
// reparse point is never traversed. The returned owner retains the final root
// handle, so replacing an ancestor path cannot redirect later artifact I/O.
func openPrivateArtifactRoot(root, workspace string, create bool) (privateArtifactRoot, string, error) {
	if !validWindowsAbsolutePath(root) || !validWindowsAbsolutePath(workspace) {
		return nil, "", errors.New("artifact private root paths are invalid")
	}

	workspaceWalk, err := walkWindowsDirectory(workspace, false)
	if err != nil {
		return nil, "", errors.New("artifact workspace root is unavailable")
	}
	defer windows.CloseHandle(workspaceWalk.handle)

	rootWalk, err := walkWindowsDirectory(root, create)
	if err != nil {
		return nil, "", errors.New("artifact private root is unavailable")
	}
	keepRoot := false
	defer func() {
		if !keepRoot {
			_ = windows.CloseHandle(rootWalk.handle)
		}
	}()

	workspaceID := workspaceWalk.identities[len(workspaceWalk.identities)-1]
	if identityInWindowsPath(workspaceID, rootWalk.identities) {
		return nil, "", errors.New("artifact private root must be outside the workspace")
	}
	if len(rootWalk.missing) == 0 {
		rootID := rootWalk.identities[len(rootWalk.identities)-1]
		if identityInWindowsPath(rootID, workspaceWalk.identities) {
			return nil, "", errors.New("artifact private root must be outside the workspace")
		}
	}

	for _, component := range rootWalk.missing {
		next, createErr := createWindowsPrivateDirectoryRelative(rootWalk.handle, component)
		if createErr != nil {
			return nil, "", errors.New("artifact private root creation failed")
		}
		_ = windows.CloseHandle(rootWalk.handle)
		rootWalk.handle = next
		identity, identityErr := windowsHandleIdentity(rootWalk.handle, true)
		if identityErr != nil {
			return nil, "", errors.New("artifact private root identity is unavailable")
		}
		rootWalk.identities = append(rootWalk.identities, identity)
	}
	if len(rootWalk.missing) > 0 {
		rootWalk.missing = nil
	}

	if err := verifyWindowsPrivateHandle(rootWalk.handle, true); err != nil {
		return nil, "", errors.New("artifact private root permissions are invalid")
	}
	rootID := rootWalk.identities[len(rootWalk.identities)-1]
	if identityInWindowsPath(workspaceID, rootWalk.identities) || identityInWindowsPath(rootID, workspaceWalk.identities) {
		return nil, "", errors.New("artifact private root must be outside the workspace")
	}
	canonical, err := windowsFinalPath(rootWalk.handle)
	if err != nil || !validWindowsAbsolutePath(canonical) {
		return nil, "", errors.New("artifact private root canonical path is unavailable")
	}

	owner := &windowsPrivateArtifactRoot{handle: rootWalk.handle}
	keepRoot = true
	return owner, canonical, nil
}

func (r *windowsPrivateArtifactRoot) create(name string) (*os.File, error) {
	if !validWindowsArtifactName(name) {
		return nil, errors.New("artifact private file name is invalid")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return nil, errors.New("artifact private root is closed")
	}
	sd, err := currentUserSecurityDescriptor(false)
	if err != nil {
		return nil, err
	}
	handle, err := ntCreateWindowsRelative(
		r.handle,
		name,
		windows.FILE_GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.DELETE,
		0,
		windows.FILE_CREATE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_ATTRIBUTE_NORMAL,
		sd,
	)
	if err != nil {
		return nil, err
	}
	if err := verifyWindowsPrivateHandle(handle, false); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	file := os.NewFile(uintptr(handle), "artifact-staging")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("artifact private file is unavailable")
	}
	return file, nil
}

func (r *windowsPrivateArtifactRoot) open(name string) (*os.File, error) {
	if !validWindowsArtifactName(name) {
		return nil, errors.New("artifact private file name is invalid")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return nil, errors.New("artifact private root is closed")
	}
	handle, err := ntCreateWindowsRelative(
		r.handle,
		name,
		windows.FILE_GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		nil,
	)
	if err != nil {
		return nil, err
	}
	if err := verifyWindowsPrivateHandle(handle, false); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	file := os.NewFile(uintptr(handle), "artifact")
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("artifact private file is unavailable")
	}
	return file, nil
}

func (r *windowsPrivateArtifactRoot) rename(oldName, newName string) error {
	if !validWindowsArtifactName(oldName) || !validWindowsArtifactName(newName) || oldName == newName {
		return errors.New("artifact private rename is invalid")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return errors.New("artifact private root is closed")
	}
	handle, err := ntCreateWindowsRelative(
		r.handle,
		oldName,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		nil,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := verifyWindowsPrivateHandle(handle, false); err != nil {
		return err
	}
	return renameWindowsHandleRelative(handle, r.handle, newName)
}

func (r *windowsPrivateArtifactRoot) remove(name string) error {
	if !validWindowsArtifactName(name) {
		return errors.New("artifact private remove is invalid")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return errors.New("artifact private root is closed")
	}
	handle, err := ntCreateWindowsRelative(
		r.handle,
		name,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		nil,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := verifyWindowsPrivateHandle(handle, false); err != nil {
		return err
	}
	var status windows.IO_STATUS_BLOCK
	disposition := byte(1)
	return windows.NtSetInformationFile(handle, &status, &disposition, 1, windows.FileDispositionInformation)
}

func (r *windowsPrivateArtifactRoot) close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handle == 0 || r.handle == windows.InvalidHandle {
		return nil
	}
	err := windows.CloseHandle(r.handle)
	r.handle = windows.InvalidHandle
	return err
}

func walkWindowsDirectory(path string, allowMissing bool) (*windowsDirectoryWalk, error) {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return nil, errors.New("Windows volume is unavailable")
	}
	volumeRoot := volume + string(filepath.Separator)
	handle, err := windows.CreateFile(
		windows.StringToUTF16Ptr(volumeRoot),
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	walk := &windowsDirectoryWalk{handle: handle}
	ok := false
	defer func() {
		if !ok {
			_ = windows.CloseHandle(walk.handle)
		}
	}()
	identity, err := windowsHandleIdentity(walk.handle, true)
	if err != nil {
		return nil, err
	}
	walk.identities = append(walk.identities, identity)

	remainder := strings.TrimLeft(path[len(volume):], `\/`)
	components := strings.FieldsFunc(remainder, func(r rune) bool { return r == '\\' || r == '/' })
	for index, component := range components {
		if !validWindowsArtifactName(component) {
			return nil, errors.New("Windows path component is invalid")
		}
		next, openErr := ntCreateWindowsRelative(
			walk.handle,
			component,
			windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.SYNCHRONIZE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_OPEN,
			windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
			0,
			nil,
		)
		if openErr != nil {
			if !allowMissing || !windowsPathIsMissing(openErr) {
				return nil, openErr
			}
			walk.missing = append(walk.missing, components[index:]...)
			ok = true
			return walk, nil
		}
		_ = windows.CloseHandle(walk.handle)
		walk.handle = next
		identity, err = windowsHandleIdentity(walk.handle, true)
		if err != nil {
			return nil, err
		}
		walk.identities = append(walk.identities, identity)
	}
	ok = true
	return walk, nil
}

func windowsPathIsMissing(err error) bool {
	return errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) ||
		errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func createWindowsPrivateDirectoryRelative(parent windows.Handle, name string) (windows.Handle, error) {
	sd, err := currentUserSecurityDescriptor(true)
	if err != nil {
		return 0, err
	}
	handle, err := ntCreateWindowsRelative(
		parent,
		name,
		windowsPrivateRootAccess,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		windows.FILE_ATTRIBUTE_DIRECTORY,
		sd,
	)
	if err != nil {
		return 0, err
	}
	if err := verifyWindowsPrivateHandle(handle, true); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

func ntCreateWindowsRelative(parent windows.Handle, name string, access, share, disposition, options, attributes uint32, sd *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	objectAttributes := windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory:      parent,
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: sd,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(
		&handle,
		access,
		&objectAttributes,
		&status,
		nil,
		attributes,
		share,
		disposition,
		options,
		0,
		0,
	); err != nil {
		return 0, err
	}
	return handle, nil
}

type windowsFileRenameInformation struct {
	replaceIfExists uint32
	rootDirectory   windows.Handle
	fileNameLength  uint32
	fileName        [1]uint16
}

func renameWindowsHandleRelative(handle, root windows.Handle, name string) error {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	encoded = encoded[:len(encoded)-1]
	var layout windowsFileRenameInformation
	bufferSize := int(unsafe.Offsetof(layout.fileName)) + len(encoded)*2
	buffer := make([]byte, bufferSize)
	information := (*windowsFileRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.rootDirectory = root
	information.fileNameLength = uint32(len(encoded) * 2)
	target := unsafe.Slice((*uint16)(unsafe.Pointer(&information.fileName[0])), len(encoded))
	copy(target, encoded)
	var status windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(handle, &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation)
}

func currentUserSecurityDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return nil, errors.New("current Windows user is unavailable")
	}
	sid := user.User.Sid.String()
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	return windows.SecurityDescriptorFromString("O:" + sid + "G:" + sid + "D:P(A;" + inheritance + ";GA;;;" + sid + ")")
}

func verifyWindowsPrivatePath(path string, directory bool) error {
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path),
		windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		flags,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return verifyWindowsPrivateHandle(handle, directory)
}

func verifyWindowsPrivateHandle(handle windows.Handle, directory bool) error {
	var fileInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &fileInfo); err != nil {
		return err
	}
	attributes := fileInfo.FileAttributes
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || directory != (attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
		return errors.New("artifact object type is invalid")
	}
	sd, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("artifact DACL is not protected")
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return errors.New("artifact owner is unavailable")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !owner.Equals(user.User.Sid) {
		return errors.New("artifact owner is not the current user")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		return errors.New("artifact DACL is not current-user-only")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		return errors.New("artifact DACL entry is invalid")
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	allRights := windows.ACCESS_MASK(windows.STANDARD_RIGHTS_ALL | windows.SPECIFIC_RIGHTS_ALL)
	hasFullAccess := ace.Mask&windows.GENERIC_ALL != 0 || ace.Mask&allRights == allRights
	if !aceSID.Equals(user.User.Sid) || !hasFullAccess {
		return errors.New("artifact DACL is not current-user-only")
	}
	return nil
}

func windowsHandleIdentity(handle windows.Handle, directory bool) (windowsFileIdentity, error) {
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return windowsFileIdentity{}, err
	}
	attributes := information.FileAttributes
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || directory != (attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
		return windowsFileIdentity{}, errors.New("artifact Windows object type is invalid")
	}
	return windowsFileIdentity{volume: information.VolumeSerialNumber, high: information.FileIndexHigh, low: information.FileIndexLow}, nil
}

func identityInWindowsPath(identity windowsFileIdentity, path []windowsFileIdentity) bool {
	for _, candidate := range path {
		if candidate == identity {
			return true
		}
	}
	return false
}

func windowsFinalPath(handle windows.Handle) (string, error) {
	size := uint32(256)
	for size <= windows.MAX_LONG_PATH {
		buffer := make([]uint16, size)
		length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], size, 0)
		if err != nil {
			return "", err
		}
		if length < size {
			path := windows.UTF16ToString(buffer[:length])
			switch {
			case strings.HasPrefix(path, `\\?\UNC\`):
				path = `\\` + strings.TrimPrefix(path, `\\?\UNC\`)
			case strings.HasPrefix(path, `\\?\`):
				path = strings.TrimPrefix(path, `\\?\`)
			}
			return filepath.Clean(path), nil
		}
		size = length + 1
	}
	return "", errors.New("artifact Windows path is too long")
}

func validWindowsAbsolutePath(path string) bool {
	return path != "" && utf8.ValidString(path) && !strings.ContainsRune(path, 0) && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validWindowsArtifactName(name string) bool {
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) &&
		!strings.ContainsAny(name, `\/:`) && !strings.ContainsRune(name, 0) &&
		!strings.HasSuffix(name, " ") && !strings.HasSuffix(name, ".")
}

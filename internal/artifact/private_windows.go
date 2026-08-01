//go:build windows

package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ensurePrivateArtifactRoot(path string) error {
	if attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path)); err == nil {
		if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.New("artifact private root is invalid")
		}
		return verifyWindowsPrivatePath(path, true)
	} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return err
	}

	current := path
	var missing []string
	for {
		parent := filepath.Dir(current)
		if parent == current {
			return errors.New("artifact private root has no existing parent")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
		attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(current))
		if err == nil {
			if attributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
				return errors.New("artifact private root parent is invalid")
			}
			break
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return err
		}
	}
	for index := len(missing) - 1; index >= 0; index-- {
		current = filepath.Join(current, missing[index])
		if err := createWindowsPrivateDirectory(current); err != nil {
			return err
		}
	}
	return verifyWindowsPrivatePath(path, true)
}

func createPrivateArtifactFile(path string) (*os.File, error) {
	sd, err := currentUserSecurityDescriptor(false)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	handle, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path),
		windows.GENERIC_WRITE|windows.READ_CONTROL,
		0,
		sa,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
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

func openPrivateArtifactFile(path string) (*os.File, error) {
	handle, err := windows.CreateFile(
		windows.StringToUTF16Ptr(path),
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
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

func createWindowsPrivateDirectory(path string) error {
	sd, err := currentUserSecurityDescriptor(true)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	if err := windows.CreateDirectory(windows.StringToUTF16Ptr(path), sa); err != nil {
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return err
		}
	}
	return verifyWindowsPrivatePath(path, true)
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

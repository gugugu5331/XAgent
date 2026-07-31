//go:build windows

package repoaudit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsACLHeader struct {
	revision  byte
	reserved  byte
	size      uint16
	aceCount  uint16
	reserved2 uint16
}

// Windows maps GENERIC_ALL to the file-object-specific access mask when a
// security descriptor is attached to a file or directory. Build and validate
// that canonical mask directly so an exact DACL survives the create/readback
// round trip.
const privateWindowsFileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

func createPrivateDirectory(path string, _ os.FileMode) error {
	security, err := privateWindowsSecurityAttributes()
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(name, security); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return fs.ErrExist
		}
		return err
	}
	return validatePrivateDirectory(path, 0)
}

func createPrivateFile(path string, _ os.FileMode, readWrite bool) (*os.File, error) {
	security, err := privateWindowsSecurityAttributes()
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.GENERIC_WRITE)
	if readWrite {
		access |= windows.GENERIC_READ
	}
	handle, err := windows.CreateFile(
		name,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		security,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return nil, fs.ErrExist
		}
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		windows.CloseHandle(handle)
		return nil, errors.New("wrap private Windows file handle")
	}
	if err := validatePrivateFileHandle(handle, false); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func privateWindowsSecurityAttributes() (*windows.SecurityAttributes, error) {
	acl, err := privateWindowsACL()
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.NewSecurityDescriptor()
	if err != nil {
		return nil, fmt.Errorf("create private security descriptor: %w", err)
	}
	if err := descriptor.SetDACL(acl, true, false); err != nil {
		return nil, fmt.Errorf("set private DACL: %w", err)
	}
	if err := descriptor.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
		return nil, fmt.Errorf("protect private DACL: %w", err)
	}
	return &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
		InheritHandle:      0,
	}, nil
}

func privateWindowsACL() (*windows.ACL, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("get current user SID: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("get local system SID: %w", err)
	}
	entries := []windows.EXPLICIT_ACCESS{
		privateWindowsAccessEntry(user.User.Sid, windows.TRUSTEE_IS_USER),
		privateWindowsAccessEntry(system, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return nil, fmt.Errorf("build private DACL: %w", err)
	}
	return acl, nil
}

func privateWindowsAccessEntry(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: privateWindowsFileAllAccess,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

func lockEvidenceLease(file *os.File) error {
	handle := windows.Handle(file.Fd())
	if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return fmt.Errorf("make evidence lease non-inheritable: %w", err)
	}
	// os.File is created without FILE_FLAG_OVERLAPPED. LockFileEx therefore blocks
	// synchronously until [0,1) is owned; ERROR_IO_PENDING is never treated as
	// successful acquisition.
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
		return fmt.Errorf("lock evidence lease byte range: %w", err)
	}
	return nil
}

func unlockEvidenceLease(file *os.File) error {
	overlapped := new(windows.Overlapped)
	if err := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, overlapped); err != nil {
		return fmt.Errorf("unlock evidence lease byte range: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.FlushFileBuffers(handle)
}

func validatePrivateDirectory(path string, _ os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("private directory type is invalid")
	}
	return validatePrivateWindowsDACL(path)
}

func validatePrivateFile(path string, _ os.FileMode, empty bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || empty && info.Size() != 0 {
		return errors.New("private file type or size is invalid")
	}
	return validatePrivateWindowsDACL(path)
}

func validatePrivateFileHandle(handle windows.Handle, empty bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("read private file information from handle: %w", err)
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || empty && (info.FileSizeHigh != 0 || info.FileSizeLow != 0) {
		return errors.New("private file handle type or size is invalid")
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read private file DACL from handle: %w", err)
	}
	return validatePrivateWindowsSecurityDescriptor(descriptor)
}

func validatePrivateWindowsDACL(path string) error {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read private DACL: %w", err)
	}
	return validatePrivateWindowsSecurityDescriptor(descriptor)
}

func validatePrivateWindowsSecurityDescriptor(descriptor *windows.SECURITY_DESCRIPTOR) error {
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read private DACL control: %w", err)
	}
	if control&windows.SE_DACL_PRESENT == 0 || control&windows.SE_DACL_PROTECTED == 0 || control&windows.SE_DACL_DEFAULTED != 0 {
		return errors.New("private DACL control is not exact")
	}
	dacl, defaulted, err := descriptor.DACL()
	if err != nil || dacl == nil || defaulted {
		return errors.New("private DACL is absent or defaulted")
	}
	header := (*windowsACLHeader)(unsafe.Pointer(dacl))
	if header.aceCount != 2 {
		return errors.New("private DACL must contain exactly two ACEs")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("get current user SID: %w", err)
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("get local system SID: %w", err)
	}
	want := map[string]bool{user.User.Sid.String(): false, system.String(): false}
	for index := uint32(0); index < 2; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("read private DACL ACE: %w", err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != privateWindowsFileAllAccess {
			return errors.New("private DACL ACE type, flags or rights are not exact")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		key := sid.String()
		seen, allowed := want[key]
		if !allowed || seen {
			return errors.New("private DACL contains an unexpected or duplicate trustee")
		}
		want[key] = true
	}
	for _, seen := range want {
		if !seen {
			return errors.New("private DACL is missing an allowed trustee")
		}
	}
	return nil
}

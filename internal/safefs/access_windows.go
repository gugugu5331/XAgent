//go:build windows

package safefs

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var windowsAccessCheck = windows.NewLazySystemDLL("advapi32.dll").NewProc("AccessCheck")

const windowsFileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

type windowsGenericMapping struct {
	read    uint32
	write   uint32
	execute uint32
	all     uint32
}

type windowsPrivilegeSet struct {
	count      uint32
	control    uint32
	privileges [64]windows.LUIDAndAttributes
}

// WindowsAccessAllowed evaluates access against the security descriptor of
// the already-open Root handle. It never exposes or re-resolves the Root path.
func (r *Root) WindowsAccessAllowed(token windows.Token, desired uint32) (bool, error) {
	if r == nil || token == 0 || desired == 0 {
		return false, errors.New("safefs Windows access request is invalid")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	backend, ok := r.backend.(*windowsRoot)
	if r.closed || !ok || backend.handle == 0 || backend.handle == windows.InvalidHandle {
		return false, errors.New("safefs Windows Root is unavailable")
	}
	descriptor, err := windows.GetSecurityInfo(
		backend.handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.GROUP_SECURITY_INFORMATION|
			windows.DACL_SECURITY_INFORMATION|windows.LABEL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false, errors.New("safefs Windows security descriptor is unavailable")
	}
	var impersonation windows.Token
	if err := windows.DuplicateTokenEx(token, windows.TOKEN_QUERY, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &impersonation); err != nil {
		return false, errors.New("safefs Windows impersonation token is unavailable")
	}
	defer impersonation.Close()
	mapping := windowsGenericMapping{
		read:    windows.FILE_GENERIC_READ,
		write:   windows.FILE_GENERIC_WRITE,
		execute: windows.FILE_GENERIC_EXECUTE,
		all:     windowsFileAllAccess,
	}
	privileges := windowsPrivilegeSet{}
	privilegeBytes := uint32(unsafe.Sizeof(privileges))
	var granted uint32
	var allowed int32
	result, _, callErr := windowsAccessCheck.Call(
		uintptr(unsafe.Pointer(descriptor)),
		uintptr(impersonation),
		uintptr(desired),
		uintptr(unsafe.Pointer(&mapping)),
		uintptr(unsafe.Pointer(&privileges)),
		uintptr(unsafe.Pointer(&privilegeBytes)),
		uintptr(unsafe.Pointer(&granted)),
		uintptr(unsafe.Pointer(&allowed)),
	)
	if result == 0 {
		return false, errors.New("safefs Windows AccessCheck failed: " + callErr.Error())
	}
	return allowed != 0 && granted&desired == desired, nil
}

//go:build windows

package proctree

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	windowsDisableMaxPrivilege = 0x1
	windowsLUAToken            = 0x4
	windowsFileAddFile         = 0x2
	windowsFileAddSubdirectory = 0x4
	windowsFileDeleteChild     = 0x40
)

var windowsCreateRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")

func createWindowsProtectedSuspendedProcess(request Request, handles windowsChildHandles) (windows.ProcessInformation, error) {
	token, err := newWindowsRestrictedLowToken()
	if err != nil {
		return windows.ProcessInformation{}, newStartError(startCodeProtectedExecUnavailable)
	}
	defer token.Close()
	if err := setWindowsScratchLowLabel(request.Protection.scratchOwner.path); err != nil {
		return windows.ProcessInformation{}, newStartError(startCodeProtectedExecUnavailable)
	}
	if err := verifyWindowsProtectionAccess(request.Protection, token); err != nil {
		return windows.ProcessInformation{}, newStartError(startCodeProtectedExecUnavailable)
	}
	info, err := createWindowsSuspendedProcessWithToken(request, handles, token)
	if err != nil {
		return windows.ProcessInformation{}, newStartError(startCodeProtectedExecUnavailable)
	}
	return info, nil
}

func newWindowsRestrictedLowToken() (windows.Token, error) {
	var source windows.Token
	access := uint32(windows.TOKEN_DUPLICATE | windows.TOKEN_ASSIGN_PRIMARY | windows.TOKEN_QUERY | windows.TOKEN_ADJUST_DEFAULT)
	if err := windows.OpenProcessToken(windows.CurrentProcess(), access, &source); err != nil {
		return 0, errors.New("proctree Windows source token is unavailable")
	}
	defer source.Close()
	user, err := source.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return 0, errors.New("proctree Windows source token identity is unavailable")
	}
	restrictingSIDs := []windows.SIDAndAttributes{{Sid: user.User.Sid}}
	for _, sidType := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinBuiltinUsersSid,
		windows.WinAuthenticatedUserSid,
		windows.WinWorldSid,
	} {
		sid, err := windows.CreateWellKnownSid(sidType)
		if err != nil {
			return 0, errors.New("proctree Windows restricting SID is unavailable")
		}
		restrictingSIDs = append(restrictingSIDs, windows.SIDAndAttributes{Sid: sid})
	}
	var restricted windows.Token
	result, _, callErr := windowsCreateRestrictedToken.Call(
		uintptr(source),
		windowsDisableMaxPrivilege|windowsLUAToken,
		0, 0,
		0, 0,
		uintptr(len(restrictingSIDs)), uintptr(unsafe.Pointer(&restrictingSIDs[0])),
		uintptr(unsafe.Pointer(&restricted)),
	)
	if result == 0 || restricted == 0 {
		return 0, errors.New("proctree Windows restricted token creation failed: " + callErr.Error())
	}
	low, err := windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		_ = restricted.Close()
		return 0, errors.New("proctree Windows low integrity SID is unavailable")
	}
	label := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{
		Sid:        low,
		Attributes: windows.SE_GROUP_INTEGRITY | windows.SE_GROUP_INTEGRITY_ENABLED,
	}}
	if err := windows.SetTokenInformation(
		restricted,
		windows.TokenIntegrityLevel,
		(*byte)(unsafe.Pointer(&label)),
		label.Size(),
	); err != nil {
		_ = restricted.Close()
		return 0, errors.New("proctree Windows low integrity setup failed")
	}
	if err := verifyWindowsRestrictedLowToken(restricted); err != nil {
		_ = restricted.Close()
		return 0, err
	}
	return restricted, nil
}

func verifyWindowsRestrictedLowToken(token windows.Token) error {
	restricted, err := token.IsRestricted()
	if err != nil || !restricted {
		return errors.New("proctree Windows token is not restricted")
	}
	buffer := make([]byte, 256)
	var size uint32
	if err := windows.GetTokenInformation(
		token,
		windows.TokenIntegrityLevel,
		&buffer[0],
		uint32(len(buffer)),
		&size,
	); err != nil {
		return errors.New("proctree Windows token integrity is unavailable")
	}
	label := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buffer[0]))
	if label.Label.Sid == nil || !label.Label.Sid.IsWellKnown(windows.WinLowLabelSid) {
		return errors.New("proctree Windows token integrity is not low")
	}
	return nil
}

func setWindowsScratchLowLabel(path string) error {
	if path == "" {
		return errors.New("proctree Windows scratch path is unavailable")
	}
	descriptor, err := windows.SecurityDescriptorFromString("S:(ML;OICI;NW;;;LW)")
	if err != nil {
		return errors.New("proctree Windows scratch label is invalid")
	}
	sacl, _, err := descriptor.SACL()
	if err != nil || sacl == nil {
		return errors.New("proctree Windows scratch label is unavailable")
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.LABEL_SECURITY_INFORMATION,
		nil, nil, nil, sacl,
	); err != nil {
		return errors.New("proctree Windows scratch label setup failed: " + err.Error())
	}
	return nil
}

func verifyWindowsProtectionAccess(plan ProtectionPlan, token windows.Token) error {
	if !plan.validForStart() || token == 0 {
		return errors.New("proctree Windows protection plan is invalid")
	}
	writeRights := []uint32{
		windows.FILE_WRITE_DATA,
		windows.FILE_APPEND_DATA,
		windows.FILE_WRITE_EA,
		windows.FILE_WRITE_ATTRIBUTES,
		windowsFileDeleteChild,
		windows.DELETE,
		windows.WRITE_DAC,
		windows.WRITE_OWNER,
	}
	for rootIndex, root := range plan.roots {
		for _, right := range writeRights {
			allowed, err := root.WindowsAccessAllowed(token, right)
			if err != nil || allowed {
				return fmt.Errorf("proctree Windows read Root access check failed: root=%d right=%#x allowed=%t: %v", rootIndex, right, allowed, err)
			}
		}
	}
	for _, right := range []uint32{windowsFileAddFile, windowsFileAddSubdirectory, windows.FILE_WRITE_DATA} {
		allowed, err := plan.scratch.WindowsAccessAllowed(token, right)
		if err != nil || !allowed {
			return fmt.Errorf("proctree Windows scratch access check failed: right=%#x allowed=%t: %v", right, allowed, err)
		}
	}
	return nil
}

func verifyWindowsProtectedProcess(request Request, process windows.Handle) error {
	if process == 0 {
		return errors.New("proctree Windows protected process is unavailable")
	}
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &token); err != nil {
		return errors.New("proctree Windows child token is unavailable")
	}
	defer token.Close()
	if err := verifyWindowsRestrictedLowToken(token); err != nil {
		return err
	}
	return verifyWindowsProtectionAccess(request.Protection, token)
}

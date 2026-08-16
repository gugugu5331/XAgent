//go:build windows

package worktree

import (
	"encoding/binary"

	"golang.org/x/sys/windows"
)

func repositoryDirectoryObjectIdentity(path string) ([]byte, error) {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, ErrUnsafePath
	}
	identity := make([]byte, 12)
	binary.LittleEndian.PutUint32(identity[:4], info.VolumeSerialNumber)
	binary.LittleEndian.PutUint32(identity[4:8], info.FileIndexHigh)
	binary.LittleEndian.PutUint32(identity[8:], info.FileIndexLow)
	return identity, nil
}

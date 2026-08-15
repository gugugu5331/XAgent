//go:build windows

package conversation

import "golang.org/x/sys/windows"

func replaceV2FileAtomic(stage string, target string) error {
	stagePath, err := windows.UTF16PtrFromString(stage)
	if err != nil {
		return err
	}
	targetPath, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(stagePath, targetPath, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

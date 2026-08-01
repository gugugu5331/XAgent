//go:build linux

package proctree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const minimumLinuxLandlockABI = 5

const linuxLandlockWriteRights uint64 = unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
	unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
	unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG |
	unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
	unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
	unix.LANDLOCK_ACCESS_FS_REFER |
	unix.LANDLOCK_ACCESS_FS_TRUNCATE |
	unix.LANDLOCK_ACCESS_FS_IOCTL_DEV

func installLinuxProtection(plan linuxLaunchPlan) error {
	// Landlock and no_new_privs are thread attributes. Keep this goroutine on
	// the restricted thread until unix.Exec replaces the launcher process.
	runtime.LockOSThread()
	abi, err := linuxLandlockABI()
	if err != nil || abi < minimumLinuxLandlockABI {
		return errLinuxProtectionUnavailable
	}
	scratchFD, err := unix.Open(
		plan.scratchPath,
		unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return errLinuxProtectionUnavailable
	}
	defer unix.Close(scratchFD)
	if err := verifyLinuxScratchHandle(scratchFD, plan.scratchIdentity); err != nil {
		return errLinuxProtectionUnavailable
	}
	ruleset := unix.LandlockRulesetAttr{Access_fs: linuxLandlockWriteRights}
	rulesetFD, err := linuxLandlockCreateRuleset(&ruleset, 0)
	if err != nil {
		return errLinuxProtectionUnavailable
	}
	defer unix.Close(rulesetFD)
	pathRule := unix.LandlockPathBeneathAttr{
		Allowed_access: linuxLandlockWriteRights,
		Parent_fd:      int32(scratchFD),
	}
	if err := linuxLandlockAddPathRule(rulesetFD, &pathRule); err != nil {
		return errLinuxProtectionUnavailable
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return errLinuxProtectionUnavailable
	}
	noNewPrivileges, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	if err != nil || noNewPrivileges != 1 {
		return errLinuxProtectionUnavailable
	}
	if err := linuxLandlockRestrictSelf(rulesetFD); err != nil {
		return errLinuxProtectionUnavailable
	}
	return nil
}

func linuxLandlockABI() (int, error) {
	version, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		0,
		0,
		uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION),
	)
	if errno != 0 || version == 0 {
		return 0, errors.New("proctree Linux Landlock ABI is unavailable")
	}
	return int(version), nil
}

func linuxLandlockCreateRuleset(attribute *unix.LandlockRulesetAttr, flags uintptr) (int, error) {
	if attribute == nil {
		return -1, errors.New("proctree Linux Landlock ruleset is invalid")
	}
	fd, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(attribute)),
		unsafe.Sizeof(*attribute),
		flags,
	)
	if errno != 0 {
		return -1, errors.New("proctree Linux Landlock ruleset creation failed")
	}
	return int(fd), nil
}

func linuxLandlockAddPathRule(rulesetFD int, attribute *unix.LandlockPathBeneathAttr) error {
	if rulesetFD < 0 || attribute == nil || attribute.Parent_fd < 0 {
		return errors.New("proctree Linux Landlock path rule is invalid")
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFD),
		uintptr(unix.LANDLOCK_RULE_PATH_BENEATH),
		uintptr(unsafe.Pointer(attribute)),
		0,
		0,
		0,
	)
	if errno != 0 {
		return errors.New("proctree Linux Landlock path rule failed")
	}
	return nil
}

func linuxLandlockRestrictSelf(rulesetFD int) error {
	if rulesetFD < 0 {
		return errors.New("proctree Linux Landlock ruleset is invalid")
	}
	_, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_RESTRICT_SELF,
		uintptr(rulesetFD),
		0,
		0,
	)
	if errno != 0 {
		return errors.New("proctree Linux Landlock restriction failed")
	}
	return nil
}

func verifyLinuxScratchHandle(fd int, expected []byte) error {
	var stat unix.Stat_t
	if fd < 0 || unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR ||
		stat.Uid != uint32(unix.Geteuid()) || stat.Mode&0o7777 != 0o700 {
		return errors.New("proctree Linux scratch handle metadata is unsafe")
	}
	actual := make([]byte, 33)
	actual[0] = 1
	binary.BigEndian.PutUint64(actual[9:17], uint64(stat.Dev))
	binary.BigEndian.PutUint64(actual[25:33], stat.Ino)
	if !bytes.Equal(actual, expected) {
		return errors.New("proctree Linux scratch handle identity changed")
	}
	return nil
}

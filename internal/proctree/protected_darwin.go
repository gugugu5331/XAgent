//go:build darwin

package proctree

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const darwinSeatbeltLauncher = "/usr/bin/sandbox-exec"

type darwinProtection struct {
	launcher string
	probe    func(context.Context, string, string, string) error
}

func newDarwinRunner(options Options) (*posixRunner, error) {
	return newDarwinRunnerWithProtection(options, darwinProtection{
		launcher: darwinSeatbeltLauncher,
		probe:    probeDarwinSeatbelt,
	})
}

func newDarwinRunnerWithProtection(options Options, protection darwinProtection) (*posixRunner, error) {
	if protection.launcher != darwinSeatbeltLauncher || protection.probe == nil {
		return nil, errors.New("proctree Darwin protection is invalid")
	}
	return newPOSIXRunner(options, protection.command)
}

func (p darwinProtection) command(ctx context.Context, request Request) (*exec.Cmd, error) {
	if request.Executable == "" || !filepath.IsAbs(request.Executable) || filepath.Clean(request.Executable) != request.Executable {
		return nil, newStartError(startCodeProtectedExecUnavailable)
	}
	if err := verifyDarwinSeatbeltLauncher(p.launcher); err != nil {
		return nil, newStartError(startCodeProtectedExecUnavailable)
	}
	scratchPath := request.Protection.scratchOwner.path
	profile, err := darwinSeatbeltProfile(scratchPath)
	if err != nil || p.probe(ctx, p.launcher, profile, scratchPath) != nil {
		return nil, newStartError(startCodeProtectedExecUnavailable)
	}
	arguments := make([]string, 0, 5+len(request.Args))
	arguments = append(arguments, "-p", profile, "--", request.Executable)
	arguments = append(arguments, request.Args...)
	command := exec.Command(p.launcher, arguments...)
	command.Env = append([]string(nil), request.Env...)
	return command, nil
}

func verifyDarwinSeatbeltLauncher(path string) error {
	if path != darwinSeatbeltLauncher {
		return errors.New("proctree Darwin launcher path is invalid")
	}
	if err := verifyDarwinSystemObject(filepath.Dir(path), true); err != nil {
		return err
	}
	return verifyDarwinSystemObject(path, false)
}

func verifyDarwinSystemObject(path string, directory bool) error {
	var named unix.Stat_t
	if err := unix.Lstat(path, &named); err != nil {
		return errors.New("proctree Darwin system object is unavailable")
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	if directory {
		flags |= unix.O_DIRECTORY
	}
	handle, err := unix.Open(path, flags, 0)
	if err != nil {
		return errors.New("proctree Darwin system object open failed")
	}
	defer unix.Close(handle)
	var opened unix.Stat_t
	if err := unix.Fstat(handle, &opened); err != nil || named.Dev != opened.Dev || named.Ino != opened.Ino {
		return errors.New("proctree Darwin system object identity changed")
	}
	wantedType := uint16(unix.S_IFREG)
	if directory {
		wantedType = uint16(unix.S_IFDIR)
	}
	if opened.Uid != 0 || opened.Mode&unix.S_IFMT != wantedType || opened.Mode&0o022 != 0 {
		return errors.New("proctree Darwin system object metadata is unsafe")
	}
	if !directory && opened.Mode&0o111 == 0 {
		return errors.New("proctree Darwin launcher is not executable")
	}
	return nil
}

func darwinSeatbeltProfile(scratchPath string) (string, error) {
	if !validDarwinSeatbeltPath(scratchPath) {
		return "", errors.New("proctree Darwin scratch path is invalid")
	}
	return `(version 1)
(allow default)
(deny file-write*
  (require-not
    (subpath "` + scratchPath + `")))`, nil
}

func validDarwinSeatbeltPath(path string) bool {
	if path == "" || !utf8.ValidString(path) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, character := range path {
		if character < 0x20 || character == 0x7f || character == '"' || character == '\\' {
			return false
		}
	}
	return true
}

func probeDarwinSeatbelt(ctx context.Context, launcher, profile, scratchPath string) error {
	allowed := filepath.Join(scratchPath, ".xagent-seatbelt-probe")
	denied := scratchPath + "-denied-probe"
	if pathExists(allowed) || pathExists(denied) {
		return errors.New("proctree Darwin probe target already exists")
	}
	defer os.Remove(allowed)
	allowedCommand := exec.CommandContext(ctx, launcher, "-p", profile, "--", "/usr/bin/touch", allowed)
	allowedCommand.Stdout = io.Discard
	allowedCommand.Stderr = io.Discard
	if err := allowedCommand.Run(); err != nil {
		return errors.New("proctree Darwin scratch probe failed")
	}
	allowedInfo, err := os.Lstat(allowed)
	if err != nil || !allowedInfo.Mode().IsRegular() {
		return errors.New("proctree Darwin scratch probe result is invalid")
	}
	if err := os.Remove(allowed); err != nil {
		return errors.New("proctree Darwin scratch probe cleanup failed")
	}
	deniedCommand := exec.CommandContext(ctx, launcher, "-p", profile, "--", "/usr/bin/touch", denied)
	deniedCommand.Stdout = io.Discard
	deniedCommand.Stderr = io.Discard
	if err := deniedCommand.Run(); err == nil {
		_ = os.Remove(denied)
		return errors.New("proctree Darwin deny probe unexpectedly succeeded")
	}
	if _, err := os.Lstat(denied); !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(denied)
		return errors.New("proctree Darwin deny probe left a target")
	}
	return nil
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

//go:build darwin || linux

package safefs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

const atomicStageAttempts = 16

func (r *posixRoot) atomicWrite(
	ctx context.Context,
	relative string,
	perm fs.FileMode,
	write func(io.Writer) error,
	validate func(bindingResolution) error,
) error {
	parentFD, owned, _, leaf, err := r.openParent(relative)
	if err != nil {
		return errors.New("safefs atomic parent open failed")
	}
	if owned {
		defer unix.Close(parentFD)
	}
	stageFD, stageName, err := createAtomicStage(parentFD)
	if err != nil {
		return errors.New("safefs atomic staging failed")
	}
	stageExists := true
	defer func() {
		if stageExists {
			_ = unix.Unlinkat(parentFD, stageName, 0)
		}
	}()
	stage := os.NewFile(uintptr(stageFD), "")
	if stage == nil {
		_ = unix.Close(stageFD)
		return errors.New("safefs atomic staging failed")
	}
	stageOpen := true
	defer func() {
		if stageOpen {
			_ = stage.Close()
		}
	}()

	select {
	case <-ctx.Done():
		return errors.New("safefs atomic write canceled")
	default:
	}
	if err := write(struct{ io.Writer }{Writer: stage}); err != nil {
		return errors.New("safefs atomic writer failed")
	}
	if err := stage.Chmod(perm); err != nil {
		return errors.New("safefs atomic staging mode failed")
	}
	if err := stage.Sync(); err != nil {
		return errors.New("safefs atomic staging sync failed")
	}
	if err := stage.Close(); err != nil {
		stageOpen = false
		return errors.New("safefs atomic staging close failed")
	}
	stageOpen = false
	select {
	case <-ctx.Done():
		return errors.New("safefs atomic write canceled")
	default:
	}
	live, err := r.bind(relative)
	if err != nil || validate(live) != nil {
		return errors.New("safefs atomic revalidation failed")
	}
	if err := unix.Renameat(parentFD, stageName, parentFD, leaf); err != nil {
		return errors.New("safefs atomic replace failed")
	}
	stageExists = false
	if err := unix.Fsync(parentFD); err != nil {
		return errors.New("safefs atomic parent sync failed")
	}
	return nil
}

func createAtomicStage(parentFD int) (int, string, error) {
	for attempt := 0; attempt < atomicStageAttempts; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return -1, "", err
		}
		name := ".xagent-stage-" + hex.EncodeToString(random[:])
		fd, err := unix.Openat(parentFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err == nil {
			return fd, name, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return -1, "", err
		}
	}
	return -1, "", errors.New("safefs atomic staging collision limit reached")
}

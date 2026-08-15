//go:build linux

package proctree

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"xagent/internal/safefs"
)

const (
	linuxLauncherEnvironment = "XAGENT_INTERNAL_LINUX_LAUNCHER"
	linuxLauncherMarker      = "inherited-plan-v1"
	linuxSelfExecutable      = "/proc/self/exe"
	linuxPlanFD              = 3
	linuxHandshakeFD         = 4
	linuxPlanVersion         = uint16(1)
	maxLinuxPlanBytes        = 256 << 10
	maxLinuxPlanStrings      = 4096
	maxLinuxPlanStringBytes  = 64 << 10
	linuxHandshakeReady      = byte('R')
	linuxHandshakeFailure    = byte('F')
)

var linuxPlanMagic = [8]byte{'X', 'A', 'G', 'L', 'N', 'X', '0', '1'}

var errLinuxProtectionUnavailable = errors.New("proctree Linux protection is unavailable")

type linuxLaunchPlan struct {
	version         uint16
	executable      string
	args            []string
	environment     []string
	scratchPath     string
	scratchIdentity []byte
	readRootCount   uint32
}

type linuxRunner struct {
	options Options
}

func newLinuxRunner(options Options) (*linuxRunner, error) {
	if options.Diagnostics == nil || options.CleanupTimeout > maxCleanupTimeout {
		return nil, errors.New("proctree Linux runner options are invalid")
	}
	return &linuxRunner{options: options}, nil
}

func (r *linuxRunner) Start(ctx context.Context, request Request) (Process, error) {
	if r == nil || ctx == nil {
		cleanupUnstartedPlan(request)
		return nil, newStartError(startCodeProcessStartFailed)
	}
	select {
	case <-ctx.Done():
		cleanupUnstartedPlan(request)
		return nil, ctx.Err()
	default:
	}
	validProtection := request.Protection.validForStart()
	if request.Mode != ProtectionRequired || !validProtection ||
		request.WorkingDir == nil || !request.Protection.containsReadRoot(request.WorkingDir) {
		if validProtection {
			_ = request.Protection.cleanupScratch()
		}
		return nil, newStartError(startCodeProtectedExecUnavailable)
	}
	prepared, err := prepareLinuxCommand(request)
	if err != nil {
		_ = request.Protection.cleanupScratch()
		return nil, newStartError(startCodeProtectedExecUnavailable)
	}
	defer prepared.closeAll()
	return startPOSIXCommandWithHandshake(
		ctx,
		prepared.command,
		request.Protection,
		r.options,
		prepared.releaseChildEnds,
		prepared.awaitExec,
	)
}

type preparedLinuxCommand struct {
	command        *exec.Cmd
	planFile       *os.File
	handshakeRead  *os.File
	handshakeWrite *os.File

	childEndsOnce sync.Once
	allOnce       sync.Once
}

func prepareLinuxCommand(request Request) (*preparedLinuxCommand, error) {
	plan, err := newLinuxLaunchPlan(request)
	if err != nil {
		return nil, err
	}
	encoded, err := encodeLinuxLaunchPlan(plan)
	if err != nil {
		return nil, err
	}
	planFile, err := createSealedLinuxPlan(encoded)
	if err != nil {
		return nil, err
	}
	handshakeRead, handshakeWrite, err := os.Pipe()
	if err != nil {
		_ = planFile.Close()
		return nil, errors.New("proctree Linux handshake creation failed")
	}
	command := exec.Command(linuxSelfExecutable)
	command.Env = replaceLinuxEnvironment(os.Environ(), linuxLauncherEnvironment, linuxLauncherMarker)
	command.ExtraFiles = []*os.File{planFile, handshakeWrite}
	return &preparedLinuxCommand{
		command:        command,
		planFile:       planFile,
		handshakeRead:  handshakeRead,
		handshakeWrite: handshakeWrite,
	}, nil
}

func (p *preparedLinuxCommand) releaseChildEnds() {
	if p == nil {
		return
	}
	p.childEndsOnce.Do(func() {
		_ = p.planFile.Close()
		_ = p.handshakeWrite.Close()
	})
}

func (p *preparedLinuxCommand) closeAll() {
	if p == nil {
		return
	}
	p.allOnce.Do(func() {
		p.releaseChildEnds()
		_ = p.handshakeRead.Close()
	})
}

func (p *preparedLinuxCommand) awaitExec(ctx context.Context) error {
	if p == nil || p.handshakeRead == nil {
		return newStartError(startCodeProtectedExecUnavailable)
	}
	return awaitLinuxExecHandshake(ctx, p.handshakeRead)
}

func awaitLinuxExecHandshake(ctx context.Context, handshake *os.File) error {
	if ctx == nil || handshake == nil {
		return newStartError(startCodeProtectedExecUnavailable)
	}
	done := make(chan error, 1)
	go func() {
		done <- readLinuxExecHandshake(handshake)
	}()
	select {
	case err := <-done:
		_ = handshake.Close()
		return err
	case <-ctx.Done():
		_ = handshake.Close()
		<-done
		return ctx.Err()
	}
}

func readLinuxExecHandshake(handshake io.Reader) error {
	var signal [1]byte
	if _, err := io.ReadFull(handshake, signal[:]); err != nil || signal[0] != linuxHandshakeReady {
		return newStartError(startCodeProtectedExecUnavailable)
	}
	n, err := handshake.Read(signal[:])
	if n == 0 && errors.Is(err, io.EOF) {
		return nil
	}
	return newStartError(startCodeProtectedExecUnavailable)
}

func newLinuxLaunchPlan(request Request) (linuxLaunchPlan, error) {
	if len(request.Protection.roots) == 0 || len(request.Protection.roots) > maxLinuxPlanStrings {
		return linuxLaunchPlan{}, errors.New("proctree Linux read root count is invalid")
	}
	identity, err := request.Protection.scratch.Identity().MarshalBinary()
	if err != nil {
		return linuxLaunchPlan{}, errors.New("proctree Linux scratch identity is unavailable")
	}
	plan := linuxLaunchPlan{
		version:         linuxPlanVersion,
		executable:      request.Executable,
		args:            append([]string(nil), request.Args...),
		environment:     append([]string(nil), request.Env...),
		scratchPath:     request.Protection.scratchOwner.path,
		scratchIdentity: append([]byte(nil), identity...),
		readRootCount:   uint32(len(request.Protection.roots)),
	}
	if !plan.valid() {
		return linuxLaunchPlan{}, errors.New("proctree Linux launch plan is invalid")
	}
	return plan, nil
}

func (p linuxLaunchPlan) valid() bool {
	if p.version != linuxPlanVersion || p.readRootCount == 0 || p.readRootCount > maxLinuxPlanStrings || len(p.scratchIdentity) != 33 ||
		!validLinuxAbsolutePath(p.executable) || !validLinuxAbsolutePath(p.scratchPath) ||
		len(p.args) > maxLinuxPlanStrings || len(p.environment) > maxLinuxPlanStrings {
		return false
	}
	for _, argument := range p.args {
		if !validLinuxPlanString(argument) {
			return false
		}
	}
	for _, item := range p.environment {
		if !validLinuxPlanString(item) || !strings.Contains(item, "=") ||
			strings.HasPrefix(item, linuxLauncherEnvironment+"=") {
			return false
		}
	}
	return true
}

func validLinuxAbsolutePath(path string) bool {
	return validLinuxPlanString(path) && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func validLinuxPlanString(value string) bool {
	return len(value) <= maxLinuxPlanStringBytes && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func encodeLinuxLaunchPlan(plan linuxLaunchPlan) ([]byte, error) {
	if !plan.valid() {
		return nil, errors.New("proctree Linux launch plan is invalid")
	}
	var encoded bytes.Buffer
	encoded.Write(linuxPlanMagic[:])
	_ = binary.Write(&encoded, binary.BigEndian, plan.version)
	_ = binary.Write(&encoded, binary.BigEndian, plan.readRootCount)
	writeLinuxPlanBytes(&encoded, []byte(plan.executable))
	writeLinuxPlanBytes(&encoded, []byte(plan.scratchPath))
	writeLinuxPlanBytes(&encoded, plan.scratchIdentity)
	_ = binary.Write(&encoded, binary.BigEndian, uint32(len(plan.args)))
	for _, argument := range plan.args {
		writeLinuxPlanBytes(&encoded, []byte(argument))
	}
	_ = binary.Write(&encoded, binary.BigEndian, uint32(len(plan.environment)))
	for _, item := range plan.environment {
		writeLinuxPlanBytes(&encoded, []byte(item))
	}
	if encoded.Len() > maxLinuxPlanBytes {
		return nil, errors.New("proctree Linux launch plan exceeds its limit")
	}
	return encoded.Bytes(), nil
}

func writeLinuxPlanBytes(encoded *bytes.Buffer, value []byte) {
	_ = binary.Write(encoded, binary.BigEndian, uint32(len(value)))
	encoded.Write(value)
}

func decodeLinuxLaunchPlan(encoded []byte) (linuxLaunchPlan, error) {
	if len(encoded) == 0 || len(encoded) > maxLinuxPlanBytes {
		return linuxLaunchPlan{}, errors.New("proctree Linux launch plan size is invalid")
	}
	reader := bytes.NewReader(encoded)
	var magic [8]byte
	if _, err := io.ReadFull(reader, magic[:]); err != nil || magic != linuxPlanMagic {
		return linuxLaunchPlan{}, errors.New("proctree Linux launch plan header is invalid")
	}
	var plan linuxLaunchPlan
	if binary.Read(reader, binary.BigEndian, &plan.version) != nil ||
		binary.Read(reader, binary.BigEndian, &plan.readRootCount) != nil {
		return linuxLaunchPlan{}, errors.New("proctree Linux launch plan header is incomplete")
	}
	executable, err := readLinuxPlanBytes(reader)
	if err != nil {
		return linuxLaunchPlan{}, err
	}
	scratchPath, err := readLinuxPlanBytes(reader)
	if err != nil {
		return linuxLaunchPlan{}, err
	}
	identity, err := readLinuxPlanBytes(reader)
	if err != nil {
		return linuxLaunchPlan{}, err
	}
	plan.executable = string(executable)
	plan.scratchPath = string(scratchPath)
	plan.scratchIdentity = identity
	plan.args, err = readLinuxPlanStrings(reader)
	if err != nil {
		return linuxLaunchPlan{}, err
	}
	plan.environment, err = readLinuxPlanStrings(reader)
	if err != nil {
		return linuxLaunchPlan{}, err
	}
	if reader.Len() != 0 || !plan.valid() {
		return linuxLaunchPlan{}, errors.New("proctree Linux launch plan payload is invalid")
	}
	return plan, nil
}

func readLinuxPlanStrings(reader *bytes.Reader) ([]string, error) {
	var count uint32
	if binary.Read(reader, binary.BigEndian, &count) != nil || count > maxLinuxPlanStrings {
		return nil, errors.New("proctree Linux launch plan list is invalid")
	}
	items := make([]string, count)
	for index := range items {
		item, err := readLinuxPlanBytes(reader)
		if err != nil {
			return nil, err
		}
		items[index] = string(item)
	}
	return items, nil
}

func readLinuxPlanBytes(reader *bytes.Reader) ([]byte, error) {
	var length uint32
	if binary.Read(reader, binary.BigEndian, &length) != nil || length > maxLinuxPlanStringBytes || uint64(length) > uint64(reader.Len()) {
		return nil, errors.New("proctree Linux launch plan field is invalid")
	}
	value := make([]byte, length)
	if _, err := io.ReadFull(reader, value); err != nil {
		return nil, errors.New("proctree Linux launch plan field is incomplete")
	}
	return value, nil
}

func createSealedLinuxPlan(encoded []byte) (*os.File, error) {
	if len(encoded) == 0 || len(encoded) > maxLinuxPlanBytes {
		return nil, errors.New("proctree Linux launch plan size is invalid")
	}
	fd, err := unix.MemfdCreate("xagent-launch-plan", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, errors.New("proctree Linux plan handle creation failed")
	}
	file := os.NewFile(uintptr(fd), "xagent-launch-plan")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("proctree Linux plan handle conversion failed")
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return nil, errors.New("proctree Linux plan handle write failed")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, errors.New("proctree Linux plan handle seek failed")
	}
	requiredSeals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, requiredSeals); err != nil {
		_ = file.Close()
		return nil, errors.New("proctree Linux plan handle sealing failed")
	}
	return file, nil
}

// RunLinuxInheritedLauncher executes the hidden launcher mode. The ordinary
// CLI can only select it through a validated inherited plan and handshake.
func RunLinuxInheritedLauncher() (bool, error) {
	if os.Getenv(linuxLauncherEnvironment) != linuxLauncherMarker {
		return false, nil
	}
	_ = os.Unsetenv(linuxLauncherEnvironment)
	if len(os.Args) != 1 {
		return true, errors.New("proctree Linux launcher arguments are invalid")
	}
	planFile := os.NewFile(linuxPlanFD, "xagent-inherited-plan")
	handshake := os.NewFile(linuxHandshakeFD, "xagent-exec-handshake")
	if planFile == nil || handshake == nil {
		return true, errors.New("proctree Linux launcher handles are unavailable")
	}
	defer planFile.Close()
	defer handshake.Close()
	return true, runLinuxInheritedLauncher(planFile, handshake, installLinuxProtection)
}

func runLinuxInheritedLauncher(planFile, handshake *os.File, install func(linuxLaunchPlan) error) (runErr error) {
	if planFile == nil || handshake == nil || install == nil {
		return errors.New("proctree Linux launcher request is invalid")
	}
	readySent := false
	defer func() {
		if recover() != nil {
			if readySent {
				_ = writeLinuxHandshake(handshake, linuxHandshakeFailure)
			}
			runErr = errors.New("proctree Linux launcher failed")
		}
	}()
	if err := validateLinuxHandshakeHandle(handshake); err != nil {
		return err
	}
	plan, err := readSealedLinuxPlan(planFile)
	if err != nil {
		return err
	}
	if err := verifyLinuxScratchIdentity(plan); err != nil {
		return err
	}
	if err := planFile.Close(); err != nil {
		return errors.New("proctree Linux plan handle close failed")
	}
	if err := writeLinuxHandshake(handshake, linuxHandshakeReady); err != nil {
		return err
	}
	readySent = true
	if err := install(plan); err != nil {
		_ = writeLinuxHandshake(handshake, linuxHandshakeFailure)
		return errLinuxProtectionUnavailable
	}
	arguments := append([]string{plan.executable}, plan.args...)
	if err := unix.Exec(plan.executable, arguments, plan.environment); err != nil {
		_ = writeLinuxHandshake(handshake, linuxHandshakeFailure)
		return errors.New("proctree Linux target exec failed")
	}
	return nil
}

func validateLinuxHandshakeHandle(handshake *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(handshake.Fd()), &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFIFO {
		return errors.New("proctree Linux handshake handle is invalid")
	}
	flags, err := unix.FcntlInt(handshake.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return errors.New("proctree Linux handshake flags are unavailable")
	}
	if _, err := unix.FcntlInt(handshake.Fd(), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return errors.New("proctree Linux handshake close-on-exec failed")
	}
	flags, err = unix.FcntlInt(handshake.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		return errors.New("proctree Linux handshake close-on-exec verification failed")
	}
	return nil
}

func readSealedLinuxPlan(file *os.File) (linuxLaunchPlan, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Size <= 0 || stat.Size > maxLinuxPlanBytes {
		return linuxLaunchPlan{}, errors.New("proctree Linux plan handle is invalid")
	}
	requiredSeals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	seals, err := unix.FcntlInt(file.Fd(), unix.F_GET_SEALS, 0)
	if err != nil || seals&requiredSeals != requiredSeals {
		return linuxLaunchPlan{}, errors.New("proctree Linux plan handle is not sealed")
	}
	encoded := make([]byte, int(stat.Size))
	if _, err := io.ReadFull(file, encoded); err != nil {
		return linuxLaunchPlan{}, errors.New("proctree Linux plan handle read failed")
	}
	return decodeLinuxLaunchPlan(encoded)
}

func verifyLinuxScratchIdentity(plan linuxLaunchPlan) error {
	opened, err := safefs.Bootstrap(plan.scratchPath, safefs.Policy{})
	if err != nil {
		return errors.New("proctree Linux scratch verification failed")
	}
	defer opened.Root.Close()
	identity, err := opened.Root.Identity().MarshalBinary()
	if err != nil || !bytes.Equal(identity, plan.scratchIdentity) {
		return errors.New("proctree Linux scratch identity changed")
	}
	return nil
}

func writeLinuxHandshake(handshake *os.File, signal byte) error {
	written, err := handshake.Write([]byte{signal})
	if err != nil || written != 1 {
		return errors.New("proctree Linux handshake write failed")
	}
	return nil
}

func replaceLinuxEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	replaced := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			continue
		}
		replaced = append(replaced, item)
	}
	return append(replaced, prefix+value)
}

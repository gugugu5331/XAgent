package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"

	"xagent/internal/command"
	"xagent/internal/orchestrator"
	"xagent/internal/redact"
	"xagent/internal/skill"
	"xagent/internal/tui"
)

const artifactPageSize int64 = 16 * 1024

var (
	errArtifactOpenUnavailable = errors.New("artifact open unavailable")
	errArtifactOpenCanceled    = errors.New("artifact open canceled")
)

// artifactReadLease is App-owned coordination for one local read. It is never
// projected into Bubble Tea messages or UI/domain state. Cancel may race with
// OpenForUser: an attached reader is actively closed, while a reader attached
// after cancellation is closed immediately. The underlying Close runs once.
type artifactReadLease struct {
	mu       sync.Mutex
	reader   io.ReadCloser
	canceled bool
	closing  bool
	closed   bool
	closeErr error
	done     chan struct{}
}

func newArtifactReadLease() *artifactReadLease {
	return &artifactReadLease{done: make(chan struct{})}
}

func (lease *artifactReadLease) attach(reader io.ReadCloser) bool {
	if lease == nil || reader == nil {
		return false
	}
	lease.mu.Lock()
	if lease.reader != nil {
		lease.mu.Unlock()
		_ = reader.Close()
		return false
	}
	lease.reader = reader
	canceled := lease.canceled
	lease.mu.Unlock()
	if canceled {
		_ = lease.Close()
		return false
	}
	return true
}

func (lease *artifactReadLease) Cancel() {
	if lease == nil {
		return
	}
	lease.mu.Lock()
	lease.canceled = true
	hasReader := lease.reader != nil
	lease.mu.Unlock()
	if hasReader {
		_ = lease.Close()
	}
}

func (lease *artifactReadLease) Canceled() bool {
	if lease == nil {
		return false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	return lease.canceled
}

func (lease *artifactReadLease) Close() error {
	if lease == nil {
		return nil
	}
	lease.mu.Lock()
	if lease.closed {
		err := lease.closeErr
		lease.mu.Unlock()
		return err
	}
	if lease.closing {
		done := lease.done
		lease.mu.Unlock()
		<-done
		lease.mu.Lock()
		err := lease.closeErr
		lease.mu.Unlock()
		return err
	}
	if lease.reader == nil {
		lease.mu.Unlock()
		return nil
	}
	lease.closing = true
	reader := lease.reader
	lease.mu.Unlock()

	err := reader.Close()
	lease.mu.Lock()
	lease.closeErr = err
	lease.closed = true
	lease.closing = false
	close(lease.done)
	lease.mu.Unlock()
	return err
}

func (c *commandController) DisplayHelp(catalog command.HelpCatalog) {
	commands := make([]tui.HelpCommandSpec, 0, len(catalog.Commands))
	for _, item := range catalog.Commands {
		commands = append(commands, tui.HelpCommandSpec{
			Name: item.Name, Aliases: append([]string(nil), item.Aliases...),
			Description: item.Description, Usage: item.Usage, Type: string(item.Type),
			ArgHint: item.ArgHint, Badge: item.Badge,
		})
	}
	shortcuts := make([]tui.HelpShortcutSpec, 0, len(catalog.Bindings))
	for _, item := range catalog.Bindings {
		shortcuts = append(shortcuts, tui.HelpShortcutSpec{
			Context: string(item.Context), Key: item.Key, Command: item.CanonicalName, Description: item.Description,
		})
	}
	entries := make([]tui.HelpEntrySpec, 0, len(catalog.Entries))
	for _, item := range catalog.Entries {
		entries = append(entries, tui.HelpEntrySpec{
			Kind: string(item.Kind), Name: item.Name, Command: item.CanonicalName, Description: item.Description,
		})
	}
	c.DisplayNotice(tui.NewHelpView(tui.HelpViewSpec{Commands: commands, Shortcuts: shortcuts, Entries: entries}).View())
}

// openArtifactForUser is the only App command-side entry that invokes the
// narrow artifact reader. It consumes one bounded, redacted page and closes
// the reader before returning. Neither raw bytes nor the reader can cross into
// a tea message or Model state.
func openArtifactForUser(
	ctx context.Context,
	artifacts ArtifactUserReader,
	intent tui.ArtifactOpenIntent,
	runtimeRedactor *redact.RuntimeRedactor,
	providedLease ...*artifactReadLease,
) (tui.ArtifactView, error) {
	id := intent.ID()
	offset := intent.Offset()
	if runtimeRedactor == nil {
		runtimeRedactor = redact.NewRuntimeRedactor()
	}
	failure := func(err error) (tui.ArtifactView, error) {
		return tui.NewArtifactView(tui.ArtifactViewSpec{
			ID: id, Offset: offset, Available: false,
			Error: runtimeRedactor.Redact("artifact 无法读取，请确认 ID 后重试"),
		}), err
	}
	if ctx == nil || ctx.Err() != nil {
		return failure(errArtifactOpenCanceled)
	}
	if artifacts == nil || !validArtifactUserID(id) || offset < 0 || offset%artifactPageSize != 0 {
		return failure(errArtifactOpenUnavailable)
	}
	lease := newArtifactReadLease()
	if len(providedLease) > 0 && providedLease[0] != nil {
		lease = providedLease[0]
	}

	reader, ref, err := artifacts.OpenForUser(ctx, id)
	if reader != nil && !lease.attach(reader) {
		return failure(errArtifactOpenCanceled)
	}
	if err != nil {
		_ = lease.Close()
		if ctx.Err() != nil || lease.Canceled() {
			return failure(errArtifactOpenCanceled)
		}
		return failure(errArtifactOpenUnavailable)
	}
	if reader == nil {
		return failure(errArtifactOpenUnavailable)
	}
	closeAndFail := func(cause error) (tui.ArtifactView, error) {
		_ = lease.Close()
		return failure(cause)
	}
	if ctx.Err() != nil || lease.Canceled() {
		return closeAndFail(errArtifactOpenCanceled)
	}
	if ref.ID != id || !validArtifactUserID(ref.ID) || ref.Bytes < 0 || ref.CreatedAt.IsZero() || !ref.Available || offset > ref.Bytes {
		return closeAndFail(errArtifactOpenUnavailable)
	}

	if offset > 0 {
		if seeker, ok := reader.(io.Seeker); ok {
			position, seekErr := seeker.Seek(offset, io.SeekStart)
			if seekErr != nil || position != offset {
				if ctx.Err() != nil || lease.Canceled() {
					return closeAndFail(errArtifactOpenCanceled)
				}
				return closeAndFail(errArtifactOpenUnavailable)
			}
		} else {
			skipped, skipErr := io.CopyN(io.Discard, reader, offset)
			if skipErr != nil || skipped != offset {
				if ctx.Err() != nil || lease.Canceled() {
					return closeAndFail(errArtifactOpenCanceled)
				}
				return closeAndFail(errArtifactOpenUnavailable)
			}
		}
	}
	if ctx.Err() != nil || lease.Canceled() {
		return closeAndFail(errArtifactOpenCanceled)
	}

	pageWithSentinel, readErr := io.ReadAll(io.LimitReader(reader, artifactPageSize+1))
	if readErr != nil {
		if ctx.Err() != nil || lease.Canceled() {
			return closeAndFail(errArtifactOpenCanceled)
		}
		return closeAndFail(errArtifactOpenUnavailable)
	}
	if ctx.Err() != nil || lease.Canceled() {
		return closeAndFail(errArtifactOpenCanceled)
	}
	remaining := ref.Bytes - offset
	consumed := int64(len(pageWithSentinel))
	hasMore := remaining > artifactPageSize
	if hasMore {
		if consumed != artifactPageSize+1 {
			return closeAndFail(errArtifactOpenUnavailable)
		}
		pageWithSentinel = pageWithSentinel[:artifactPageSize]
		consumed = artifactPageSize
	} else if consumed != remaining {
		return closeAndFail(errArtifactOpenUnavailable)
	}
	if closeErr := lease.Close(); closeErr != nil {
		if ctx.Err() != nil || lease.Canceled() {
			return failure(errArtifactOpenCanceled)
		}
		return failure(errArtifactOpenUnavailable)
	}
	return tui.NewArtifactView(tui.ArtifactViewSpec{
		ID: ref.ID, Bytes: ref.Bytes, CreatedAtUnixNano: ref.CreatedAt.UnixNano(),
		Available: ref.Available, Complete: ref.Complete,
		Page: runtimeRedactor.Redact(string(pageWithSentinel)), Offset: offset,
		ConsumedBytes: consumed, HasMore: hasMore,
	}), nil
}

func validArtifactUserID(id string) bool {
	if len(id) != 64 || strings.TrimSpace(id) != id {
		return false
	}
	for _, current := range id {
		if !((current >= '0' && current <= '9') || (current >= 'a' && current <= 'f')) {
			return false
		}
	}
	return true
}

func (m *Model) dispatchInput() tea.Cmd {
	if !m.acceptsIntent() {
		return nil
	}
	_ = m.refreshSkillCommands(context.Background())
	controller := &commandController{model: m}
	result := m.ensureCommandRegistry().Dispatch(m.input.Value(), controller)
	switch result.Kind {
	case command.DispatchEmpty:
		m.status.Error = nil
		m.exposeSkillNotice()
		return nil
	case command.DispatchPlainText:
		return m.submitUserMessage(result.Text)
	case command.DispatchExecuted, command.DispatchUnknown:
		m.input.Clear()
		m.commandMenu.Close()
		m.exposeSkillNotice()
		return controller.cmd
	default:
		return nil
	}
}

func (m *Model) submitUserMessage(text string) tea.Cmd {
	if !m.acceptsIntent() {
		return nil
	}
	_ = m.refreshSkillCommands(context.Background())
	text = sanitizeInput(text)
	if text == "" {
		m.status.Error = nil
		return nil
	}
	if m.conversation == nil {
		m.startNewConversation()
	}
	if m.conversation == nil {
		m.exposeSkillNotice()
		return nil
	}
	if m.skillActivity == nil {
		m.skillActivity = skill.NewActivity()
	}
	if m.orchestrator == nil {
		err := m.redactError(fmt.Errorf("Orchestrator 未启用"))
		m.status.Error = err
		m.lastError = err
		m.exposeSkillNotice()
		return nil
	}
	profile, err := m.orchestrator.BuildExecutionProfile(m.mode, m.skillActivity, 0)
	if err != nil {
		safe := m.redactError(err)
		m.status.Error = safe
		m.lastError = safe
		m.exposeSkillNotice()
		return nil
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	events, err := m.orchestrator.SendRequest(requestCtx, m.conversation, orchestrator.RunRequest{
		UserText: text, Mode: m.mode, Profile: profile, Activity: m.skillActivity,
	})
	if err != nil {
		cancel()
		safe := m.redactError(err)
		m.status.Error = safe
		m.lastError = safe
		m.exposeSkillNotice()
		return nil
	}
	requestModel := ""
	if len(profile.Activity.Active) > 0 {
		requestModel = profile.Model
	}
	cmd := m.beginRequest(cancel, events, requestModel, false)
	m.syncSkillStatus()
	m.exposeSkillNotice()
	return cmd
}

func (m *Model) completeCommand() {
	_ = m.refreshSkillCommands(context.Background())
	suggestions := m.ensureCommandRegistry().Complete(m.input.Value())
	if len(suggestions) == 0 {
		m.commandMenu.Close()
		(&commandController{model: m}).DisplayNotice("没有匹配的命令；请使用 /help 查看可用命令")
		m.exposeSkillNotice()
		return
	}
	if len(suggestions) == 1 {
		m.input.SetValue("/" + suggestions[0].Name)
		m.commandMenu.Close()
		m.exposeSkillNotice()
		return
	}
	items := make([]tui.CommandMenuItem, 0, len(suggestions))
	for _, suggestion := range suggestions {
		items = append(items, tui.CommandMenuItem{Name: suggestion.Name, Description: suggestion.Description, ArgHint: suggestion.ArgHint, Badge: suggestion.Badge})
	}
	m.commandMenu.Open(items)
	m.exposeSkillNotice()
}

func (m *Model) ensureCommandRegistry() *command.Registry {
	if m.commandRegistry == nil {
		m.ensureBaseCommands()
		m.commandRegistry = command.MustNew(m.baseCommands...)
		if m.deps.SkillManager != nil {
			_ = m.installSkillSnapshot(m.deps.SkillManager.Snapshot())
		}
	}
	return m.commandRegistry
}

func (m *Model) acceptCommandCompletion() {
	item, ok := m.commandMenu.SelectedItem()
	if !ok {
		m.commandMenu.Close()
		return
	}
	m.input.SetValue("/" + item.Name)
	m.commandMenu.Close()
}

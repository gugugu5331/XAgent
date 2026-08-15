package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"xagent/internal/diagnostics"
	"xagent/internal/redact"
)

const (
	resultMessageSchemaMaxBytes    int64 = 64 << 10
	resultTruncationReasonMaxBytes       = 256
)

// ResultInboxOptions supplies the bounded storage policy and the same identity
// generator used by the task manager. The inbox uses the generator for claim
// IDs; task and notification IDs are validated again at this trust boundary.
type ResultInboxOptions struct {
	Limits      Limits
	IDGenerator func() (ID, error)
}

type resultInbox struct {
	mu sync.Mutex

	limits      Limits
	idGenerator func() (ID, error)
	closed      bool
	closeErr    error

	reservations         map[ID]resultReservation
	releasedReservations map[ID]ParentRef
	releasedOrder        []ID
	pending              map[string][]*storedResult
	publishedByTask      map[ID]*storedResult
	pendingCount         int
	totalBytes           int64

	leasesByConversation map[string]*resultLease
	leasesByID           map[string]*resultLease
	settledClaims        map[string]settledResultClaim
	settledOrder         []string

	activeIdentifiers map[string]identifierKind
	recentIdentifiers map[string]struct{}
	recentOrder       []string
	historyLimit      int
}

type resultReservation struct {
	parent ParentRef
}

type storedResult struct {
	notification ResultNotification
	serialized   []byte
	bytes        int64
}

type resultLease struct {
	id              string
	owner           ParentRef
	conversationID  string
	items           []*storedResult
	serializedBytes int64
	done            chan struct{}
}

type claimDisposition uint8

const (
	claimReleased claimDisposition = iota + 1
	claimConsumed
)

type settledResultClaim struct {
	owner       ParentRef
	disposition claimDisposition
}

type identifierKind uint8

const (
	identifierTask identifierKind = iota + 1
	identifierNotification
	identifierClaim
)

// NewResultInbox constructs a process-local, bounded result inbox. Limits are
// validated as one coherent policy so reservations can guarantee that an
// in-limit Publish cannot later lose a capacity race.
func NewResultInbox(options ResultInboxOptions) (*resultInbox, error) {
	if options.IDGenerator == nil {
		return nil, newResultInboxError(ErrInternal, "result inbox ID generator is unavailable", false)
	}
	if err := options.Limits.Validate(); err != nil {
		return nil, newResultInboxError(ErrInternal, "result inbox limits are invalid", false)
	}
	historyLimit := options.Limits.MaxPendingResults
	if historyLimit < options.Limits.MaxResultsPerClaim*2 {
		historyLimit = options.Limits.MaxResultsPerClaim * 2
	}
	if historyLimit < 64 {
		historyLimit = 64
	}
	return &resultInbox{
		limits:               options.Limits,
		idGenerator:          options.IDGenerator,
		reservations:         make(map[ID]resultReservation),
		releasedReservations: make(map[ID]ParentRef),
		pending:              make(map[string][]*storedResult),
		publishedByTask:      make(map[ID]*storedResult),
		leasesByConversation: make(map[string]*resultLease),
		leasesByID:           make(map[string]*resultLease),
		settledClaims:        make(map[string]settledResultClaim),
		activeIdentifiers:    make(map[string]identifierKind),
		recentIdentifiers:    make(map[string]struct{}),
		historyLimit:         historyLimit,
	}, nil
}

// MarshalResultMessage projects one notification to the only model-visible
// schema. Struct field order is intentional and is part of the byte-budget
// contract shared with the orchestrator and Provider adapters.
func MarshalResultMessage(notification ResultNotification) ([]byte, error) {
	return marshalResultMessage(notification, DefaultLimits().MaxIDBytes)
}

func (inbox *resultInbox) Reserve(ctx context.Context, taskID ID, parent ParentRef) error {
	if inbox == nil {
		return newResultInboxError(ErrShutdown, "result inbox is unavailable", false)
	}
	if ctx == nil {
		return newResultInboxError(ErrInvalidTransition, "result reservation context is invalid", true)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}

	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if err := inbox.closedErrorLocked(); err != nil {
		return err
	}
	if !validBoundedResultID(string(taskID), inbox.limits.MaxIDBytes) {
		return newResultInboxError(ErrInvalidTask, "result reservation task identity is invalid", true)
	}
	if !validResultSourceParent(parent, inbox.limits.MaxIDBytes) {
		return newResultInboxError(ErrInvalidParent, "result reservation parent is invalid", true)
	}
	if inbox.identifierUsedLocked(string(taskID)) {
		return newResultInboxError(ErrInvalidTransition, "result reservation identity was already used", true)
	}
	if inbox.pendingCount >= inbox.limits.MaxPendingResults ||
		inbox.totalBytes > inbox.limits.MaxResultTotalBytes-inbox.limits.MaxResultBytes {
		return newResultInboxError(ErrInboxFull, "result inbox capacity is full", true)
	}

	inbox.reservations[taskID] = resultReservation{parent: parent}
	inbox.pendingCount++
	inbox.totalBytes += inbox.limits.MaxResultBytes
	inbox.activeIdentifiers[string(taskID)] = identifierTask
	return nil
}

func (inbox *resultInbox) ReleaseReservation(ctx context.Context, taskID ID, parent ParentRef) error {
	if inbox == nil {
		return newResultInboxError(ErrShutdown, "result inbox is unavailable", false)
	}
	if ctx == nil {
		return newResultInboxError(ErrInvalidTransition, "result reservation context is invalid", true)
	}

	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if err := inbox.closedErrorLocked(); err != nil {
		return err
	}
	if !validBoundedResultID(string(taskID), inbox.limits.MaxIDBytes) {
		return newResultInboxError(ErrInvalidTask, "result reservation task identity is invalid", true)
	}
	if !validResultSourceParent(parent, inbox.limits.MaxIDBytes) {
		return newResultInboxError(ErrInvalidParent, "result reservation parent is invalid", true)
	}
	if reservation, exists := inbox.reservations[taskID]; exists {
		if reservation.parent != parent {
			return newResultInboxError(ErrInvalidTransition, "result reservation owner does not match", true)
		}
		delete(inbox.reservations, taskID)
		inbox.pendingCount--
		inbox.totalBytes -= inbox.limits.MaxResultBytes
		delete(inbox.activeIdentifiers, string(taskID))
		inbox.rememberIdentifierLocked(string(taskID))
		inbox.rememberReleasedReservationLocked(taskID, parent)
		return nil
	}
	if published, exists := inbox.publishedByTask[taskID]; exists {
		if published.notification.Parent != parent {
			return newResultInboxError(ErrInvalidTransition, "published result owner does not match", true)
		}
		return nil
	}
	if releasedParent, exists := inbox.releasedReservations[taskID]; exists && releasedParent != parent {
		return newResultInboxError(ErrInvalidTransition, "released reservation owner does not match", true)
	}
	// Absence is intentionally a no-op: cleanup paths can safely retry after a
	// failed prepare, a successful foreground completion, or a prior cleanup.
	return nil
}

func (inbox *resultInbox) Publish(ctx context.Context, notification ResultNotification) error {
	if inbox == nil {
		return newResultInboxError(ErrShutdown, "result inbox is unavailable", false)
	}
	if ctx == nil {
		return newResultInboxError(ErrInvalidTransition, "result publish context is invalid", true)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	payload, projectionErr := marshalResultMessage(notification, inbox.limits.MaxIDBytes)
	if projectionErr != nil {
		return projectionErr
	}
	serializedBytes := int64(len(payload))

	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if err := inbox.closedErrorLocked(); err != nil {
		return err
	}
	reservation, exists := inbox.reservations[notification.TaskID]
	if !exists {
		return newResultInboxError(ErrInvalidTransition, "result was not reserved", true)
	}
	if reservation.parent != notification.Parent {
		return newResultInboxError(ErrInvalidTransition, "result parent does not match its reservation", true)
	}
	if serializedBytes > inbox.limits.MaxResultBytes {
		return newResultInboxError(ErrInboxFull, "result exceeds the single-result capacity", true)
	}
	if inbox.identifierUsedLocked(notification.NotificationID) {
		return newResultInboxError(ErrInvalidTransition, "result notification identity was already used", true)
	}

	stored := &storedResult{
		notification: notification.Clone(),
		serialized:   append([]byte(nil), payload...),
		bytes:        serializedBytes,
	}
	delete(inbox.reservations, notification.TaskID)
	inbox.totalBytes -= inbox.limits.MaxResultBytes - serializedBytes
	inbox.pending[notification.Parent.ConversationID] = insertStoredResult(
		inbox.pending[notification.Parent.ConversationID], stored,
	)
	inbox.publishedByTask[notification.TaskID] = stored
	inbox.activeIdentifiers[notification.NotificationID] = identifierNotification
	return nil
}

func (inbox *resultInbox) Claim(ctx context.Context, options ResultClaimOptions) (ResultClaim, error) {
	if inbox == nil {
		return ResultClaim{}, newResultInboxError(ErrShutdown, "result inbox is unavailable", false)
	}
	if ctx == nil {
		return ResultClaim{}, newResultInboxError(ErrInvalidTransition, "result claim context is invalid", true)
	}
	if err := context.Cause(ctx); err != nil {
		return ResultClaim{}, err
	}

	inbox.mu.Lock()
	if err := inbox.closedErrorLocked(); err != nil {
		inbox.mu.Unlock()
		return ResultClaim{}, err
	}
	if !validResultClaimOwner(options.Owner, inbox.limits.MaxIDBytes) {
		inbox.mu.Unlock()
		return ResultClaim{}, newResultInboxError(ErrInvalidParent, "result claim owner is invalid", true)
	}
	if options.MaxNotifications <= 0 || options.MaxBytes <= 0 {
		inbox.mu.Unlock()
		return ResultClaim{}, newResultInboxError(ErrInvalidTransition, "result claim limits are invalid", true)
	}
	if _, leased := inbox.leasesByConversation[options.Owner.ConversationID]; leased {
		inbox.mu.Unlock()
		return ResultClaim{}, newResultInboxError(ErrInvalidTransition, "conversation results already have an active lease", true)
	}

	maximum := options.MaxNotifications
	if maximum > inbox.limits.MaxResultsPerClaim {
		maximum = inbox.limits.MaxResultsPerClaim
	}
	available := inbox.pending[options.Owner.ConversationID]
	selected := make([]*storedResult, 0, min(maximum, len(available)))
	var serializedBytes int64
	for _, candidate := range available {
		if len(selected) >= maximum || candidate.bytes > options.MaxBytes-serializedBytes {
			break
		}
		selected = append(selected, candidate)
		serializedBytes += candidate.bytes
	}
	if len(selected) == 0 {
		inbox.mu.Unlock()
		return ResultClaim{Owner: options.Owner}, nil
	}

	generated, generationErr := inbox.idGenerator()
	if generationErr != nil || !validBoundedResultID(string(generated), inbox.limits.MaxIDBytes) || inbox.identifierUsedLocked(string(generated)) {
		inbox.mu.Unlock()
		return ResultClaim{}, newResultInboxError(ErrInternal, "result claim identity generation failed", false)
	}
	claimID := string(generated)
	lease := &resultLease{
		id:              claimID,
		owner:           options.Owner,
		conversationID:  options.Owner.ConversationID,
		items:           append([]*storedResult(nil), selected...),
		serializedBytes: serializedBytes,
		done:            make(chan struct{}),
	}
	inbox.leasesByConversation[lease.conversationID] = lease
	inbox.leasesByID[claimID] = lease
	inbox.activeIdentifiers[claimID] = identifierClaim
	claim := cloneLeaseClaim(lease)
	inbox.mu.Unlock()

	go inbox.releaseLeaseWhenContextEnds(ctx, claimID, options.Owner, lease.done)
	return claim, nil
}

func (inbox *resultInbox) Ack(ctx context.Context, claimID string, owner ParentRef) error {
	if inbox == nil {
		return newResultInboxError(ErrShutdown, "result inbox is unavailable", false)
	}
	if ctx == nil {
		return newResultInboxError(ErrInvalidTransition, "result acknowledgement context is invalid", true)
	}

	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if err := inbox.closedErrorLocked(); err != nil {
		return err
	}
	lease, err := inbox.matchLeaseLocked(claimID, owner, false)
	if err != nil {
		return err
	}

	claimed := make(map[*storedResult]struct{}, len(lease.items))
	for _, item := range lease.items {
		claimed[item] = struct{}{}
		delete(inbox.publishedByTask, item.notification.TaskID)
		delete(inbox.activeIdentifiers, string(item.notification.TaskID))
		delete(inbox.activeIdentifiers, item.notification.NotificationID)
		inbox.rememberIdentifierLocked(string(item.notification.TaskID))
		inbox.rememberIdentifierLocked(item.notification.NotificationID)
		inbox.pendingCount--
		inbox.totalBytes -= item.bytes
	}
	retained := inbox.pending[lease.conversationID][:0]
	for _, item := range inbox.pending[lease.conversationID] {
		if _, consumed := claimed[item]; !consumed {
			retained = append(retained, item)
		}
	}
	if len(retained) == 0 {
		delete(inbox.pending, lease.conversationID)
	} else {
		inbox.pending[lease.conversationID] = retained
	}
	inbox.finishLeaseLocked(lease, claimConsumed)
	return nil
}

func (inbox *resultInbox) Release(ctx context.Context, claimID string, owner ParentRef) error {
	if inbox == nil {
		return newResultInboxError(ErrShutdown, "result inbox is unavailable", false)
	}
	if ctx == nil {
		return newResultInboxError(ErrInvalidTransition, "result release context is invalid", true)
	}

	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if err := inbox.closedErrorLocked(); err != nil {
		return err
	}
	lease, err := inbox.matchLeaseLocked(claimID, owner, true)
	if err != nil || lease == nil {
		return err
	}
	inbox.finishLeaseLocked(lease, claimReleased)
	return nil
}

// Close stops claim-cancellation watchers and rejects future operations. It is
// intentionally a concrete optional lifecycle hook rather than part of the
// ResultInbox interface described by the orchestration contract.
func (inbox *resultInbox) Close(cause error) {
	if inbox == nil {
		return
	}
	if cause == nil {
		cause = newResultInboxError(ErrShutdown, "result inbox was closed", false)
	}
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if inbox.closed {
		return
	}
	inbox.closed = true
	inbox.closeErr = cloneResultInboxError(cause)
	for _, lease := range inbox.leasesByID {
		close(lease.done)
	}
	inbox.reservations = nil
	inbox.pending = nil
	inbox.publishedByTask = nil
	inbox.leasesByConversation = nil
	inbox.leasesByID = nil
	inbox.releasedReservations = nil
	inbox.releasedOrder = nil
	inbox.settledClaims = nil
	inbox.settledOrder = nil
	inbox.activeIdentifiers = nil
	inbox.recentIdentifiers = nil
	inbox.recentOrder = nil
	inbox.pendingCount = 0
	inbox.totalBytes = 0
}

func (inbox *resultInbox) releaseLeaseWhenContextEnds(ctx context.Context, claimID string, owner ParentRef, done <-chan struct{}) {
	select {
	case <-ctx.Done():
		_ = inbox.Release(context.Background(), claimID, owner)
	case <-done:
	}
}

func (inbox *resultInbox) matchLeaseLocked(claimID string, owner ParentRef, releasing bool) (*resultLease, error) {
	if !validBoundedResultID(claimID, inbox.limits.MaxIDBytes) || !validResultClaimOwner(owner, inbox.limits.MaxIDBytes) {
		return nil, newResultInboxError(ErrInvalidTransition, "result claim identity is invalid", true)
	}
	if lease, exists := inbox.leasesByID[claimID]; exists {
		if lease.owner != owner {
			return nil, newResultInboxError(ErrInvalidTransition, "result claim owner does not match", true)
		}
		return lease, nil
	}
	if settled, exists := inbox.settledClaims[claimID]; exists {
		if settled.owner != owner {
			return nil, newResultInboxError(ErrInvalidTransition, "result claim owner does not match", true)
		}
		if settled.disposition == claimReleased && releasing {
			return nil, nil
		}
		if settled.disposition == claimConsumed {
			return nil, newResultInboxError(ErrAlreadyConsumed, "result claim was already consumed", true)
		}
		return nil, newResultInboxError(ErrInvalidTransition, "result claim is no longer active", true)
	}
	return nil, newResultInboxError(ErrInvalidTransition, "result claim was not found", true)
}

func (inbox *resultInbox) finishLeaseLocked(lease *resultLease, disposition claimDisposition) {
	delete(inbox.leasesByConversation, lease.conversationID)
	delete(inbox.leasesByID, lease.id)
	delete(inbox.activeIdentifiers, lease.id)
	inbox.rememberIdentifierLocked(lease.id)
	close(lease.done)
	inbox.rememberSettledClaimLocked(lease.id, settledResultClaim{owner: lease.owner, disposition: disposition})
}

func (inbox *resultInbox) rememberSettledClaimLocked(claimID string, settled settledResultClaim) {
	if _, exists := inbox.settledClaims[claimID]; !exists {
		inbox.settledOrder = append(inbox.settledOrder, claimID)
	}
	inbox.settledClaims[claimID] = settled
	for len(inbox.settledOrder) > inbox.historyLimit {
		oldest := inbox.settledOrder[0]
		inbox.settledOrder = inbox.settledOrder[1:]
		delete(inbox.settledClaims, oldest)
	}
}

func (inbox *resultInbox) rememberReleasedReservationLocked(taskID ID, parent ParentRef) {
	if _, exists := inbox.releasedReservations[taskID]; !exists {
		inbox.releasedOrder = append(inbox.releasedOrder, taskID)
	}
	inbox.releasedReservations[taskID] = parent
	for len(inbox.releasedOrder) > inbox.historyLimit {
		oldest := inbox.releasedOrder[0]
		inbox.releasedOrder = inbox.releasedOrder[1:]
		delete(inbox.releasedReservations, oldest)
	}
}

func (inbox *resultInbox) identifierUsedLocked(identifier string) bool {
	if _, active := inbox.activeIdentifiers[identifier]; active {
		return true
	}
	_, recent := inbox.recentIdentifiers[identifier]
	return recent
}

func (inbox *resultInbox) rememberIdentifierLocked(identifier string) {
	if identifier == "" {
		return
	}
	if _, exists := inbox.recentIdentifiers[identifier]; !exists {
		inbox.recentOrder = append(inbox.recentOrder, identifier)
	}
	inbox.recentIdentifiers[identifier] = struct{}{}
	for len(inbox.recentOrder) > inbox.historyLimit {
		oldest := inbox.recentOrder[0]
		inbox.recentOrder = inbox.recentOrder[1:]
		delete(inbox.recentIdentifiers, oldest)
	}
}

func (inbox *resultInbox) closedErrorLocked() error {
	if !inbox.closed {
		return nil
	}
	return cloneResultInboxError(inbox.closeErr)
}

func insertStoredResult(results []*storedResult, candidate *storedResult) []*storedResult {
	index := sort.Search(len(results), func(index int) bool {
		return !storedResultLess(results[index], candidate)
	})
	results = append(results, nil)
	copy(results[index+1:], results[index:])
	results[index] = candidate
	return results
}

func storedResultLess(left, right *storedResult) bool {
	if left.notification.CompletionRevision != right.notification.CompletionRevision {
		return left.notification.CompletionRevision < right.notification.CompletionRevision
	}
	return left.notification.TaskID < right.notification.TaskID
}

func cloneLeaseClaim(lease *resultLease) ResultClaim {
	claim := ResultClaim{
		ClaimID:         lease.id,
		Owner:           lease.owner,
		SerializedBytes: lease.serializedBytes,
		Notifications:   make([]ResultNotification, len(lease.items)),
	}
	for index := range lease.items {
		claim.Notifications[index] = lease.items[index].notification.Clone()
	}
	return claim
}

type resultMessage struct {
	SchemaVersion    int                 `json:"schema_version"`
	TaskID           string              `json:"task_id"`
	Status           string              `json:"status"`
	Summary          string              `json:"summary"`
	SummaryTruncated bool                `json:"summary_truncated"`
	TruncationReason string              `json:"truncation_reason"`
	StopReason       string              `json:"stop_reason"`
	Usage            resultMessageUsage  `json:"usage"`
	Error            *resultMessageError `json:"error"`
}

type resultMessageUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type resultMessageError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable,omitempty"`
}

func marshalResultMessage(notification ResultNotification, maxIDBytes int64) ([]byte, error) {
	if maxIDBytes > DefaultLimits().MaxIDBytes {
		maxIDBytes = DefaultLimits().MaxIDBytes
	}
	if !validBoundedResultID(notification.NotificationID, maxIDBytes) ||
		!validBoundedResultID(string(notification.TaskID), maxIDBytes) {
		return nil, newResultInboxError(ErrInvalidTransition, "result notification identity is invalid", true)
	}
	if !validResultSourceParent(notification.Parent, maxIDBytes) || notification.CompletionRevision == 0 ||
		notification.CompletionSequence == 0 || notification.CreatedAt.IsZero() ||
		!utf8.ValidString(notification.Summary.Text()) || !utf8.ValidString(notification.TruncationReason.Text()) ||
		len(notification.TruncationReason.Text()) > resultTruncationReasonMaxBytes {
		return nil, newResultInboxError(ErrInvalidTransition, "result notification is invalid", true)
	}
	completion := Completion{
		ID:               notification.TaskID,
		Status:           notification.Status,
		Summary:          notification.Summary,
		SummaryTruncated: notification.SummaryTruncated,
		TruncationReason: notification.TruncationReason,
		StopReason:       notification.StopReason,
		Usage:            notification.Usage,
		Error:            notification.Error,
		EndedAt:          notification.CreatedAt,
	}
	if err := completion.Validate(); err != nil {
		return nil, newResultInboxError(ErrInvalidTransition, "result terminal projection is invalid", true)
	}

	message := resultMessage{
		SchemaVersion:    1,
		TaskID:           string(notification.TaskID),
		Status:           string(notification.Status),
		Summary:          notification.Summary.Text(),
		SummaryTruncated: notification.SummaryTruncated,
		TruncationReason: notification.TruncationReason.Text(),
		StopReason:       string(notification.StopReason),
		Usage: resultMessageUsage{
			InputTokens:              notification.Usage.InputTokens,
			OutputTokens:             notification.Usage.OutputTokens,
			CacheCreationInputTokens: notification.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     notification.Usage.CacheReadInputTokens,
		},
	}
	if notification.Error != nil {
		if notification.Error.Source != "subagent" ||
			!validBoundedResultID(notification.Error.Code, maxIDBytes) ||
			!utf8.ValidString(notification.Error.Message.Text()) {
			return nil, newResultInboxError(ErrInvalidTransition, "result safe error is invalid", true)
		}
		message.Error = &resultMessageError{
			Code:        notification.Error.Code,
			Message:     notification.Error.Message.Text(),
			Recoverable: notification.Error.Recoverable,
		}
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, newResultInboxError(ErrInternal, "result projection serialization failed", false)
	}
	if int64(len(payload)) > resultMessageSchemaMaxBytes {
		return nil, newResultInboxError(ErrInboxFull, "result exceeds the fixed schema capacity", true)
	}
	return payload, nil
}

func validResultSourceParent(parent ParentRef, maxIDBytes int64) bool {
	if !validBoundedResultID(parent.ConversationID, maxIDBytes) {
		return false
	}
	return parent.ExecutionID == "" || validBoundedResultID(parent.ExecutionID, maxIDBytes)
}

func validResultClaimOwner(parent ParentRef, maxIDBytes int64) bool {
	return validBoundedResultID(parent.ConversationID, maxIDBytes) &&
		validBoundedResultID(parent.ExecutionID, maxIDBytes) && parent.RequestGeneration > 0
}

func validBoundedResultID(value string, maxBytes int64) bool {
	return int64(len(value)) <= maxBytes && utf8.ValidString(value) && strings.TrimSpace(value) == value && validIdentifier(value)
}

func newResultInboxError(code ErrorCode, message string, recoverable bool) error {
	return SafeError(code, redact.NewRuntimeRedactor().Redact(message), recoverable)
}

func cloneResultInboxError(source error) error {
	if source == nil {
		return nil
	}
	var safe *diagnostics.SafeError
	if errors.As(source, &safe) {
		return cloneSafeError(safe)
	}
	return source
}

var _ ResultInbox = (*resultInbox)(nil)

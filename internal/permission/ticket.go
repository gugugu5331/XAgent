package permission

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"sync"
	"time"
)

const (
	executionTicketVersion uint16        = 1
	defaultTicketLifetime  time.Duration = time.Minute
	maxNonceAttempts                     = 16
)

const ticketMACDomain = "xagent.permission.execution-ticket"

var errTicketSerialization = errors.New("execution tickets cannot be serialized")

// TicketIssuer signs a fresh execution capability for one call ID and call
// identity. Rules and confirmations use this interface; they never construct
// ExecutionTicket values directly.
type TicketIssuer interface {
	Issue(callID string, identity CallIdentity) (ExecutionTicket, error)
}

// TicketVerifier atomically validates and consumes an execution capability at
// the actual tool start boundary.
type TicketVerifier interface {
	VerifyAndConsume(ticket ExecutionTicket, callID string, identity CallIdentity) error
}

// ExecutionTicket is an opaque, process-local, single-use capability. All
// fields remain private and common serialization paths explicitly fail.
type ExecutionTicket struct {
	version       uint16
	nonce         [32]byte
	callID        string
	identity      CallIdentity
	expiresAt     int64
	safetyEpoch   uint64
	authenticator [32]byte
}

// Issued reports whether a ticket has the structural fields produced by a
// TicketIssuer. It does not authenticate or consume the ticket.
func (t ExecutionTicket) Issued() bool {
	return t.version == executionTicketVersion && t.nonce != ([32]byte{}) && t.callID != "" && t.identity.version == callIdentityVersion
}

func (ExecutionTicket) MarshalJSON() ([]byte, error) {
	return nil, errTicketSerialization
}

func (*ExecutionTicket) UnmarshalJSON([]byte) error {
	return errTicketSerialization
}

func (ExecutionTicket) MarshalBinary() ([]byte, error) {
	return nil, errTicketSerialization
}

func (*ExecutionTicket) UnmarshalBinary([]byte) error {
	return errTicketSerialization
}

// TicketAuthority owns the process-local signing key, issued nonce set, and
// safety epoch. It implements both TicketIssuer and TicketVerifier.
type TicketAuthority struct {
	mu sync.Mutex

	secret      [32]byte
	issued      map[[32]byte]time.Time
	safetyEpoch uint64
	lifetime    time.Duration
	now         func() time.Time
	entropy     io.Reader
}

// NewTicketAuthority creates an authority with a fresh process-local key.
func NewTicketAuthority() (*TicketAuthority, error) {
	authority := &TicketAuthority{
		issued:      make(map[[32]byte]time.Time),
		safetyEpoch: 1,
		lifetime:    defaultTicketLifetime,
		now:         time.Now,
		entropy:     rand.Reader,
	}
	if _, err := io.ReadFull(authority.entropy, authority.secret[:]); err != nil {
		return nil, errors.New("create ticket authority failed")
	}
	return authority, nil
}

// Issue signs a new ticket even when the call ID and identity match a previous
// issue. The nonce set makes accidental entropy reuse fail closed.
func (a *TicketAuthority) Issue(callID string, identity CallIdentity) (ExecutionTicket, error) {
	if a == nil || callID == "" || identity.version != callIdentityVersion {
		return ExecutionTicket{}, errors.New("ticket issue input is invalid")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.now == nil || a.entropy == nil || a.lifetime <= 0 || a.issued == nil || a.secret == ([32]byte{}) {
		return ExecutionTicket{}, errors.New("ticket authority is unavailable")
	}
	issuedAt := a.now()
	a.removeExpiredLocked(issuedAt)

	var nonce [32]byte
	available := false
	for range maxNonceAttempts {
		if _, err := io.ReadFull(a.entropy, nonce[:]); err != nil {
			return ExecutionTicket{}, errors.New("issue ticket nonce failed")
		}
		if nonce == ([32]byte{}) {
			continue
		}
		if _, duplicate := a.issued[nonce]; !duplicate {
			available = true
			break
		}
	}
	if !available {
		return ExecutionTicket{}, errors.New("issue unique ticket nonce failed")
	}

	expiresAt := issuedAt.Add(a.lifetime)
	if !expiresAt.After(issuedAt) {
		return ExecutionTicket{}, errors.New("ticket lifetime is invalid")
	}
	ticket := ExecutionTicket{
		version:     executionTicketVersion,
		nonce:       nonce,
		callID:      callID,
		identity:    identity,
		expiresAt:   expiresAt.UnixNano(),
		safetyEpoch: a.safetyEpoch,
	}
	ticket.authenticator = ticketMAC(a.secret, ticket)
	a.issued[nonce] = expiresAt
	return ticket, nil
}

// VerifyAndConsume performs every check and the successful nonce deletion
// while holding one lock. Failed call-ID or identity checks do not consume an
// otherwise valid ticket; expired and stale-epoch tickets can never recover.
func (a *TicketAuthority) VerifyAndConsume(ticket ExecutionTicket, callID string, identity CallIdentity) error {
	if a == nil {
		return errors.New("ticket authority is unavailable")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.now == nil || a.issued == nil || a.secret == ([32]byte{}) {
		return errors.New("ticket authority is unavailable")
	}
	if ticket.version != executionTicketVersion ||
		ticket.nonce == ([32]byte{}) ||
		ticket.callID != callID ||
		ticket.identity != identity ||
		identity.version != callIdentityVersion {
		return errors.New("execution ticket is invalid")
	}
	issuedExpiry, issued := a.issued[ticket.nonce]
	if !issued || issuedExpiry.UnixNano() != ticket.expiresAt {
		return errors.New("execution ticket is invalid")
	}
	if !a.now().Before(issuedExpiry) {
		delete(a.issued, ticket.nonce)
		return errors.New("execution ticket has expired")
	}
	if ticket.safetyEpoch != a.safetyEpoch {
		delete(a.issued, ticket.nonce)
		return errors.New("execution ticket safety epoch is stale")
	}
	expected := ticketMAC(a.secret, ticket)
	if !hmac.Equal(ticket.authenticator[:], expected[:]) {
		return errors.New("execution ticket is invalid")
	}
	delete(a.issued, ticket.nonce)
	return nil
}

// AdvanceSafetyEpoch invalidates every outstanding ticket. It is safe to call
// concurrently and only makes the permission state more restrictive.
func (a *TicketAuthority) AdvanceSafetyEpoch() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.safetyEpoch++
	clear(a.issued)
	a.mu.Unlock()
}

func (a *TicketAuthority) removeExpiredLocked(now time.Time) {
	for nonce, expiresAt := range a.issued {
		if !now.Before(expiresAt) {
			delete(a.issued, nonce)
		}
	}
}

func ticketMAC(secret [32]byte, ticket ExecutionTicket) [32]byte {
	var encoded bytes.Buffer
	writeIdentityUint16(&encoded, ticket.version)
	writeIdentityBytes(&encoded, []byte(ticketMACDomain))
	writeIdentityBytes(&encoded, ticket.nonce[:])
	writeIdentityBytes(&encoded, []byte(ticket.callID))
	writeIdentityUint16(&encoded, ticket.identity.version)
	writeIdentityBytes(&encoded, ticket.identity.digest[:])
	writeIdentityUint64(&encoded, uint64(ticket.expiresAt))
	writeIdentityUint64(&encoded, ticket.safetyEpoch)

	digest := hmac.New(sha256.New, secret[:])
	_, _ = digest.Write(encoded.Bytes())
	var result [32]byte
	copy(result[:], digest.Sum(nil))
	return result
}

var (
	_ TicketIssuer   = (*TicketAuthority)(nil)
	_ TicketVerifier = (*TicketAuthority)(nil)
)

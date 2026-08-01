package permission

import (
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTicketCanBeConsumedExactlyOnce(t *testing.T) {
	authority := mustTicketAuthority(t)
	identity := mustTicketIdentity(t, `{"path":"first.txt"}`)

	ticket, err := authority.Issue("call-once", identity)
	if err != nil {
		t.Fatalf("issue one-time ticket: %v", err)
	}
	start := make(chan struct{})
	var successes atomic.Int32
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if authority.VerifyAndConsume(ticket, "call-once", identity) == nil {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wait.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("ticket consumption successes=%d, want 1", got)
	}
	if err := authority.VerifyAndConsume(ticket, "call-once", identity); err == nil {
		t.Fatal("consumed ticket was replayed")
	}
	if err := authority.VerifyAndConsume(ExecutionTicket{}, "call-once", identity); err == nil {
		t.Fatal("zero ticket was accepted")
	}

	retryable, err := authority.Issue("retryable", identity)
	if err != nil {
		t.Fatalf("issue validation ticket: %v", err)
	}
	otherIdentity := mustTicketIdentity(t, `{"path":"second.txt"}`)
	if err := authority.VerifyAndConsume(retryable, "wrong-call", identity); err == nil {
		t.Fatal("ticket accepted the wrong call ID")
	}
	if err := authority.VerifyAndConsume(retryable, "retryable", otherIdentity); err == nil {
		t.Fatal("ticket accepted the wrong call identity")
	}
	if err := authority.VerifyAndConsume(retryable, "retryable", identity); err != nil {
		t.Fatalf("failed verification consumed a valid ticket: %v", err)
	}

	now := time.Date(2026, 8, 2, 1, 2, 3, 0, time.UTC)
	authority.now = func() time.Time { return now }
	authority.lifetime = time.Second
	expired, err := authority.Issue("expired", identity)
	if err != nil {
		t.Fatalf("issue expiring ticket: %v", err)
	}
	now = now.Add(time.Second)
	if err := authority.VerifyAndConsume(expired, "expired", identity); err == nil {
		t.Fatal("expired ticket was accepted")
	}

	now = now.Add(-time.Second)
	staleEpoch, err := authority.Issue("stale-epoch", identity)
	if err != nil {
		t.Fatalf("issue epoch-bound ticket: %v", err)
	}
	authority.AdvanceSafetyEpoch()
	if err := authority.VerifyAndConsume(staleEpoch, "stale-epoch", identity); err == nil {
		t.Fatal("ticket survived a safety epoch change")
	}
	freshEpoch, err := authority.Issue("fresh-epoch", identity)
	if err != nil {
		t.Fatalf("issue ticket after epoch change: %v", err)
	}
	if err := authority.VerifyAndConsume(freshEpoch, "fresh-epoch", identity); err != nil {
		t.Fatalf("fresh epoch ticket was rejected: %v", err)
	}
}

func TestFreshIssueUsesNewNonce(t *testing.T) {
	authority := mustTicketAuthority(t)
	identity := mustTicketIdentity(t, `{}`)
	first, err := authority.Issue("same-call", identity)
	if err != nil {
		t.Fatalf("issue first ticket: %v", err)
	}
	second, err := authority.Issue("same-call", identity)
	if err != nil {
		t.Fatalf("issue second ticket: %v", err)
	}
	if first.nonce == second.nonce || first == second {
		t.Fatal("fresh issue reused an earlier nonce or ticket")
	}
	if err := authority.VerifyAndConsume(first, "same-call", identity); err != nil {
		t.Fatalf("consume first independently issued ticket: %v", err)
	}
	if err := authority.VerifyAndConsume(second, "same-call", identity); err != nil {
		t.Fatalf("consume second independently issued ticket: %v", err)
	}
}

func TestTicketIsNotSerializable(t *testing.T) {
	authority := mustTicketAuthority(t)
	identity := mustTicketIdentity(t, `{}`)
	ticket, err := authority.Issue("private-ticket", identity)
	if err != nil {
		t.Fatalf("issue ticket: %v", err)
	}
	if encoded, err := json.Marshal(ticket); err == nil || len(encoded) != 0 {
		t.Fatalf("ticket JSON serialization succeeded: bytes=%q err=%v", encoded, err)
	}
	if encoded, err := ticket.MarshalBinary(); err == nil || len(encoded) != 0 {
		t.Fatalf("ticket binary serialization succeeded: bytes=%x err=%v", encoded, err)
	}
	if err := json.Unmarshal([]byte(`{}`), &ticket); err == nil {
		t.Fatal("ticket JSON deserialization succeeded")
	}
	ticketType := reflect.TypeOf(ticket)
	for index := range ticketType.NumField() {
		if ticketType.Field(index).IsExported() {
			t.Fatalf("execution ticket exposes serializable field %q", ticketType.Field(index).Name)
		}
	}
}

func TestCallIDIsBoundOutsideIdentity(t *testing.T) {
	authority := mustTicketAuthority(t)
	identity := mustTicketIdentity(t, `{"command":"printf ok"}`)
	ticket, err := authority.Issue("call-a", identity)
	if err != nil {
		t.Fatalf("issue call-ID-bound ticket: %v", err)
	}
	if ticket.identity != identity {
		t.Fatal("ticket did not retain the separately constructed call identity")
	}
	if err := authority.VerifyAndConsume(ticket, "call-b", identity); err == nil {
		t.Fatal("ticket crossed call IDs while the identity remained equal")
	}
	if err := authority.VerifyAndConsume(ticket, "call-a", identity); err != nil {
		t.Fatalf("wrong call ID attempt consumed the ticket: %v", err)
	}
}

func mustTicketAuthority(t *testing.T) *TicketAuthority {
	t.Helper()
	authority, err := NewTicketAuthority()
	if err != nil {
		t.Fatalf("create ticket authority: %v", err)
	}
	return authority
}

func mustTicketIdentity(t *testing.T, arguments string) CallIdentity {
	t.Helper()
	identity, err := NewCallIdentity(CallIdentityInput{
		ToolName:           "Example",
		CanonicalArguments: []byte(arguments),
	})
	if err != nil {
		t.Fatalf("create call identity: %v", err)
	}
	return identity
}

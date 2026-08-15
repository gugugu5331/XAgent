package testutil

import (
	"context"
	"sync"
	"time"

	"xagent/internal/conversation"
	"xagent/internal/redact"
)

// ScriptedStore is a scriptable implementation of the single Store
// contract. It deliberately does not derive revisions or save progress from
// message counts; tests that care about SaveResult must provide SaveFunc.
type ScriptedStore struct {
	CreateFunc   func(context.Context) (*conversation.Conversation, error)
	ListFunc     func(context.Context) (conversation.ListResult, error)
	LoadFunc     func(context.Context, string) (conversation.LoadResult, error)
	SaveFunc     func(context.Context, *conversation.Conversation) (conversation.SaveResult, error)
	MaintainFunc func(context.Context) (conversation.MaintenanceResult, error)

	mu            sync.Mutex
	createCalls   int
	listCalls     int
	loadCalls     []string
	saveCalls     []*conversation.Conversation
	maintainCalls int
}

var _ conversation.Store = (*ScriptedStore)(nil)

func (s *ScriptedStore) Create(ctx context.Context) (*conversation.Conversation, error) {
	s.mu.Lock()
	s.createCalls++
	call := s.createCalls
	fn := s.CreateFunc
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return NewConversation("test-session-"+decimal(call), time.Unix(int64(call), 0)), nil
}

func (s *ScriptedStore) List(ctx context.Context) (conversation.ListResult, error) {
	s.mu.Lock()
	s.listCalls++
	fn := s.ListFunc
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return conversation.ListResult{}, nil
}

func (s *ScriptedStore) Load(ctx context.Context, id string) (conversation.LoadResult, error) {
	s.mu.Lock()
	s.loadCalls = append(s.loadCalls, id)
	fn := s.LoadFunc
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, id)
	}
	return conversation.LoadResult{}, nil
}

func (s *ScriptedStore) Save(ctx context.Context, value *conversation.Conversation) (conversation.SaveResult, error) {
	cloned := CloneConversation(value)
	s.mu.Lock()
	s.saveCalls = append(s.saveCalls, cloned)
	fn := s.SaveFunc
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, value)
	}
	return conversation.SaveResult{Kind: conversation.SaveNoop}, nil
}

func (s *ScriptedStore) Maintain(ctx context.Context) (conversation.MaintenanceResult, error) {
	s.mu.Lock()
	s.maintainCalls++
	fn := s.MaintainFunc
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return conversation.MaintenanceResult{}, nil
}

type StoreCalls struct {
	Create   int
	List     int
	Load     []string
	Save     []*conversation.Conversation
	Maintain int
}

func (s *ScriptedStore) Calls() StoreCalls {
	if s == nil {
		return StoreCalls{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := StoreCalls{
		Create:   s.createCalls,
		List:     s.listCalls,
		Load:     append([]string(nil), s.loadCalls...),
		Maintain: s.maintainCalls,
		Save:     make([]*conversation.Conversation, len(s.saveCalls)),
	}
	for index, value := range s.saveCalls {
		result.Save[index] = CloneConversation(value)
	}
	return result
}

func NewConversation(id string, now time.Time) *conversation.Conversation {
	redactor := redact.NewRuntimeRedactor()
	return &conversation.Conversation{
		ID:        id,
		Title:     redactor.Redact("New conversation"),
		Messages:  []conversation.Message{},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func CloneConversation(source *conversation.Conversation) *conversation.Conversation {
	if source == nil {
		return nil
	}
	cloned := *source
	cloned.Messages = append([]conversation.Message(nil), source.Messages...)
	for index := range cloned.Messages {
		if source.Messages[index].Tool == nil {
			continue
		}
		toolState := *source.Messages[index].Tool
		if source.Messages[index].Tool.Artifact != nil {
			artifactRef := *source.Messages[index].Tool.Artifact
			toolState.Artifact = &artifactRef
		}
		if source.Messages[index].Tool.Error != nil {
			safeError := *source.Messages[index].Tool.Error
			toolState.Error = &safeError
		}
		cloned.Messages[index].Tool = &toolState
	}
	if source.Context != nil {
		metadata := *source.Context
		if source.Context.LastCompressionAt != nil {
			value := *source.Context.LastCompressionAt
			metadata.LastCompressionAt = &value
		}
		cloned.Context = &metadata
	}
	return &cloned
}

func decimal(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[position:])
}

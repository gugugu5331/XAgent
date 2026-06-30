package conversation

import "context"

type ConversationStore interface {
	List(ctx context.Context) ([]Conversation, error)
	Load(ctx context.Context, id string) (*Conversation, error)
	Save(ctx context.Context, conversation *Conversation) error
	Create(ctx context.Context) (*Conversation, error)
}

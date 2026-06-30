package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type FileStore struct {
	dataDir string
}

func NewFileStore(dataDir string) (*FileStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fmt.Errorf("会话数据目录不能为空")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("无法创建会话数据目录: %w", err)
	}
	return &FileStore{dataDir: dataDir}, nil
}

func (s *FileStore) Create(ctx context.Context) (*Conversation, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return NewConversation(newID(), time.Now()), nil
}

func (s *FileStore) Save(ctx context.Context, conversation *Conversation) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if conversation == nil {
		return fmt.Errorf("无法保存空会话")
	}
	if strings.TrimSpace(conversation.ID) == "" {
		return fmt.Errorf("无法保存缺少 ID 的会话")
	}
	conversation.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(conversation, "", "  ")
	if err != nil {
		return fmt.Errorf("会话序列化失败: %w", err)
	}
	path := s.path(conversation.ID)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("保存会话失败: %w", err)
	}
	return nil
}

func (s *FileStore) Load(ctx context.Context, id string) (*Conversation, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return nil, fmt.Errorf("加载会话失败: %w", err)
	}
	var conversation Conversation
	if err := json.Unmarshal(data, &conversation); err != nil {
		return nil, fmt.Errorf("会话文件格式无效: %w", err)
	}
	return &conversation, nil
}

func (s *FileStore) List(ctx context.Context) ([]Conversation, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return nil, fmt.Errorf("读取会话列表失败: %w", err)
	}
	conversations := make([]Conversation, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dataDir, entry.Name()))
		if err != nil {
			continue
		}
		var conversation Conversation
		if err := json.Unmarshal(data, &conversation); err != nil {
			continue
		}
		conversations = append(conversations, conversation)
	}
	sort.Slice(conversations, func(i, j int) bool {
		return conversations[i].UpdatedAt.After(conversations[j].UpdatedAt)
	})
	return conversations, nil
}

func (s *FileStore) path(id string) string {
	name := filepath.Base(id) + ".json"
	return filepath.Join(s.dataDir, name)
}

func newID() string {
	return fmt.Sprintf("conv_%d", time.Now().UnixNano())
}

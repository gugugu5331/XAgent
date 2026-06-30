package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"xagent/internal/config"
	"xagent/internal/conversation"
	"xagent/internal/orchestrator"
	"xagent/internal/provider"
	"xagent/internal/resources"
)

func main() {
	cfg, err := config.Load(".claude/mock-config.yaml")
	if err != nil {
		log.Fatal(err)
	}
	store, err := conversation.NewFileStore(cfg.Storage.DataDir)
	if err != nil {
		log.Fatal(err)
	}
	llm, err := provider.New(cfg.LLM)
	if err != nil {
		log.Fatal(err)
	}
	conv, err := store.Create(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	orch := orchestrator.New(llm, store, resources.New(), cfg.LLM.Thinking)
	events, err := orch.Send(context.Background(), conv, "你好")
	if err != nil {
		log.Fatal(err)
	}
	var text strings.Builder
	for event := range events {
		switch event.Type {
		case "text_delta":
			fmt.Print(event.Text)
			text.WriteString(event.Text)
		case "done":
			fmt.Printf("\nDURATION=%s\n", event.Duration)
		case "error":
			log.Fatal(event.Err)
		}
	}
	if !strings.Contains(text.String(), "流式回复") {
		log.Fatalf("unexpected response: %q", text.String())
	}
}

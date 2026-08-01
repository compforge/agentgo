package main

import (
	"context"
	"fmt"
	"os"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/llm"
	"github.com/compforge/agentgo/tools"
)

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY not set")
		os.Exit(1)
	}

	model, err := llm.NewModel("openai", "gpt-5-mini", llm.WithAPIKey(apiKey))
	if err != nil {
		fmt.Fprintf(os.Stderr, "model error: %v\n", err)
		os.Exit(1)
	}

	// Shared file read state — Read records stamps that Write/Edit use to
	// enforce read-before-write and detect stale writes.
	fileState := tools.NewFileReadState()

	agent := agentgo.NewAgent(
		agentgo.WithModel(model),
		agentgo.WithSystemPrompt("You are a helpful coding assistant. Use the provided tools to help users."),
		agentgo.WithTools(
			tools.NewRead(".", fileState),
			tools.NewWrite(".", fileState),
			tools.NewEdit(".", fileState),
			tools.NewBash("."),
		),
		agentgo.WithMaxTurns(20),
	)

	// Subscribe to events for output
	agent.Subscribe(func(ev agentgo.Event) {
		switch ev.Type {
		case agentgo.EventMessageEnd:
			if ev.Message != nil && ev.Message.GetRole() == agentgo.RoleAssistant {
				fmt.Printf("\nAssistant: %s\n", ev.Message.TextContent())
			}
		case agentgo.EventToolExecStart:
			fmt.Printf("  [tool] %s(%s)\n", ev.Tool, string(ev.Args))
		case agentgo.EventToolExecUpdate:
			if ev.Progress != nil {
				fmt.Printf("  [progress:%s] %s\n", ev.Progress.Kind, formatProgress(ev.Progress))
				break
			}
			switch ev.UpdateKind {
			case agentgo.ToolExecUpdatePreview:
				fmt.Printf("  [preview] %s\n", string(ev.Result))
			case agentgo.ToolExecUpdateProgress:
				fmt.Printf("  [progress] %s\n", string(ev.Result))
			default:
				fmt.Printf("  [update] %s\n", string(ev.Result))
			}
		case agentgo.EventToolExecEnd:
			if ev.IsError {
				fmt.Printf("  [tool] %s error\n", ev.Tool)
			}
		case agentgo.EventError:
			fmt.Fprintf(os.Stderr, "Error: %v\n", ev.Err)
		}
	})

	if err := agent.Prompt(context.Background(), "List the files in the current directory and tell me what you see."); err != nil {
		fmt.Fprintf(os.Stderr, "prompt error: %v\n", err)
		os.Exit(1)
	}

	agent.WaitForIdle()
}

func formatProgress(progress *agentgo.ProgressPayload) string {
	if progress == nil {
		return ""
	}
	if progress.Summary != "" {
		return progress.Summary
	}
	if progress.Tool != "" {
		return progress.Tool
	}
	if progress.Message != "" {
		return progress.Message
	}
	if progress.Delta != "" {
		return progress.Delta
	}
	return string(progress.Kind)
}

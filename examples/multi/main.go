package main

import (
	"context"
	"fmt"
	"os"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/llm"
	"github.com/compforge/agentgo/subagent"
	"github.com/compforge/agentgo/tools"
)

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY not set")
		os.Exit(1)
	}

	mainModel, err := llm.NewModel("openai", "gpt-5-mini", llm.WithAPIKey(apiKey))
	if err != nil {
		fmt.Fprintf(os.Stderr, "model error: %v\n", err)
		os.Exit(1)
	}
	scoutModel, err := llm.NewModel("openai", "gpt-5-mini", llm.WithAPIKey(apiKey))
	if err != nil {
		fmt.Fprintf(os.Stderr, "model error: %v\n", err)
		os.Exit(1)
	}

	// One file read state per agent. Sub-agents get their own independent
	// state since they have their own conversation history.
	mainState := tools.NewFileReadState()
	scoutState := tools.NewFileReadState()
	reviewerState := tools.NewFileReadState()

	// Define sub-agent configurations (like pi's .md agent files)
	scout := subagent.Config{
		Name:         "scout",
		Description:  "Fast codebase reconnaissance",
		Model:        scoutModel,
		SystemPrompt: "You are a scout agent. Quickly explore the codebase and report what you find. Be concise.",
		Tools: []agentgo.Tool{
			tools.NewRead(".", scoutState),
			tools.NewBash("."),
		},
		MaxTurns: 5,
	}

	reviewer := subagent.Config{
		Name:         "reviewer",
		Description:  "Code review specialist",
		Model:        mainModel,
		SystemPrompt: "You are a code reviewer. Review the code and provide constructive feedback on quality, style, and correctness.",
		Tools: []agentgo.Tool{
			tools.NewRead(".", reviewerState),
			tools.NewBash("."),
		},
		MaxTurns: 5,
	}

	// Main agent has the subagent tool — it delegates to scout/reviewer
	agent := agentgo.NewAgent(
		agentgo.WithModel(mainModel),
		agentgo.WithSystemPrompt(
			"You are a coding assistant. Use the subagent tool to delegate tasks:\n"+
				"- Use 'scout' for codebase exploration\n"+
				"- Use 'reviewer' for code review\n"+
				"You can use chain mode to scout first, then review based on findings.",
		),
		agentgo.WithTools(
			tools.NewRead(".", mainState),
			tools.NewWrite(".", mainState),
			tools.NewEdit(".", mainState),
			tools.NewBash("."),
			subagent.NewRunner(scout, reviewer).AsTool(),
		),
		agentgo.WithMaxTurns(20),
	)

	agent.Subscribe(func(ev agentgo.Event) {
		switch ev.Type {
		case agentgo.EventMessageEnd:
			if ev.Message != nil && ev.Message.GetRole() == agentgo.RoleAssistant {
				fmt.Printf("\nAssistant: %s\n", ev.Message.TextContent())
			}
		case agentgo.EventToolExecStart:
			fmt.Printf("  [tool] %s\n", ev.Tool)
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
		case agentgo.EventError:
			fmt.Fprintf(os.Stderr, "Error: %v\n", ev.Err)
		}
	})

	if err := agent.Prompt(context.Background(), "Explore the current directory structure, then review any Go files you find. Use chain mode: scout first, then review."); err != nil {
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

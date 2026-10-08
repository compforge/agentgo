// Run with go run ./examples/artifacts. The scripted model needs no API key.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/compforge/agentgo"
	agentcontext "github.com/compforge/agentgo/context"
)

// These types and the association between them belong to the application.
type fileArtifact struct {
	Path    string
	Content string
}

func (f fileArtifact) ID() string   { return f.Path }
func (f fileArtifact) Kind() string { return "file" }

type fileMessage struct {
	agentgo.Message
	File fileArtifact
}

func (m fileMessage) Raw() agentgo.AgentMessage { return m }

func userFile(file fileArtifact) fileMessage {
	return fileMessage{Message: agentgo.UserMsg(file.Content), File: file}
}

// A view preserves the source domain message and the tool result's metadata.
type fileView struct {
	agentgo.AgentMessage
	text string
}

func (m fileView) TextContent() string { return m.text }
func (m fileView) ToMessage() (agentgo.Message, bool) {
	message, ok := m.AgentMessage.ToMessage()
	message.Content = []agentgo.ContentBlock{agentgo.TextBlock(m.text)}
	return message, ok
}

func registerInitial(artifacts agentgo.ArtifactManager) agentgo.BeforeRunHook {
	return func(_ context.Context, run agentgo.BeforeRunContext) (agentgo.AgentSnapshot, error) {
		for _, messages := range [][]agentgo.AgentMessage{run.Snapshot.State.Messages, run.Input} {
			for _, message := range messages {
				file, ok := message.Raw().(fileMessage)
				if !ok {
					continue
				}
				// This example treats history as a seed. It must not overwrite
				// material that a tool has subsequently updated in the manager.
				if _, exists := artifacts.GetArtifact(file.File.ID()); exists {
					continue
				}
				if err := artifacts.AddArtifact(file.File, false); err != nil {
					return agentgo.AgentSnapshot{}, err
				}
			}
		}
		return run.Snapshot, nil
	}
}

func fileTransformer(artifacts agentgo.ArtifactManager) agentcontext.Transformer {
	return agentcontext.TransformFunc(func(_ context.Context, messages []agentgo.AgentMessage) ([]agentgo.AgentMessage, error) {
		// Coverage belongs to this request. Compaction may remove the earlier
		// occurrence, so never use a permanent "already shown" flag on Artifact.
		seen := make(map[string]bool)
		view := append([]agentgo.AgentMessage(nil), messages...)
		for i, message := range messages {
			source, ok := message.Raw().(fileMessage)
			if !ok {
				continue
			}
			artifact, exists := artifacts.GetArtifact(source.File.ID())
			if !exists {
				continue
			}
			file, ok := artifact.(fileArtifact)
			if !ok {
				continue
			}
			text := "File reference: " + file.Path
			if !seen[file.ID()] {
				text = "File: " + file.Path + "\n" + file.Content
				seen[file.ID()] = true
			}
			view[i] = fileView{AgentMessage: message.Raw(), text: text}
		}
		return view, nil
	})
}

func artifactOptions(artifacts agentgo.ArtifactManager) []agentgo.AgentOption {
	tool := agentgo.NewFuncTool("load_report", "Load the review report", map[string]any{
		"type": "object", "properties": map[string]any{},
	}, func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(fileArtifact{Path: "report.md", Content: "Review report: checks passed."})
	})
	registerResult := func(ctx context.Context, execution agentgo.ToolExecution, next agentgo.ToolExecuteFunc) (agentgo.ToolResult, error) {
		result, err := next(ctx, execution)
		if err != nil || result.IsError || execution.Call.Name != "load_report" {
			return result, err
		}
		var file fileArtifact
		if err := json.Unmarshal(result.Content, &file); err != nil {
			return result, err
		}
		if err := artifacts.AddArtifact(file, true); err != nil {
			return result, err
		}
		result.Details = file
		return result, nil
	}
	return []agentgo.AgentOption{
		agentgo.WithBeforeRun(registerInitial(artifacts)),
		agentgo.WithTools(tool),
		agentgo.WithToolMiddlewares(registerResult),
		agentgo.WithToolResultMessageFactory(func(call agentgo.ToolCall, result agentgo.ToolResult) agentgo.AgentMessage {
			file, ok := result.Details.(fileArtifact)
			if !ok || result.IsError {
				return nil
			} // Keep the default error message.
			return fileMessage{
				Message: agentgo.ToolResultMsg(call.ID, result.Content, false),
				File:    file,
			}
		}),
		agentgo.WithContextManager(agentcontext.NewEngine(agentcontext.EngineConfig{
			ContextWindow: 32000,
			Transformers:  []agentcontext.Transformer{fileTransformer(artifacts)},
		})),
	}
}

func main() {
	artifacts := agentgo.NewArtifactManager()
	model := &demoModel{}
	options := append(artifactOptions(artifacts), agentgo.WithModel(model))
	agent := agentgo.NewAgent(options...)
	agent.Subscribe(func(event agentgo.Event) {
		if event.Type == agentgo.EventError {
			log.Printf("agent: %v", event.Err)
		}
	})
	file := fileArtifact{Path: "main.go", Content: "package main\nfunc main() {}"}
	// The caller supplies only messages. The Run hook extracts their artifacts.
	if err := agent.PromptMessages(context.Background(), userFile(file), userFile(file)); err != nil {
		log.Fatal(err)
	}
	agent.WaitForIdle()
	for i, messages := range model.requests {
		fmt.Printf("Request %d:\n", i+1)
		for _, message := range messages {
			fmt.Printf("  %s: %s\n", message.Role, message.TextContent())
		}
	}
	fmt.Printf("Registered artifacts: %d\n", len(artifacts.ListArtifacts()))
}

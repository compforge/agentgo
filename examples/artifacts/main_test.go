package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/compforge/agentgo"
)

type retryError struct{}

func (retryError) Error() string             { return "temporary model failure" }
func (retryError) Retryable() bool           { return true }
func (retryError) RetryAfter() time.Duration { return time.Millisecond }

func TestArtifactRunLifecycle(t *testing.T) {
	artifacts := agentgo.NewArtifactManager()
	model := &demoModel{}
	beforeCalls, attempts := 0, 0
	var runErr error
	options := append(artifactOptions(artifacts),
		agentgo.WithModel(model),
		agentgo.WithMaxRetries(1),
		agentgo.WithBeforeRun(func(ctx context.Context, run agentgo.BeforeRunContext) (agentgo.AgentSnapshot, error) {
			beforeCalls++
			return registerInitial(artifacts)(ctx, run)
		}),
		agentgo.WithModelMiddlewares(func(ctx context.Context, execution agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
			attempts++
			if _, ok := artifacts.GetArtifact("main.go"); !ok {
				t.Error("initial artifact missing before model middleware")
			}
			if attempts == 1 {
				return agentgo.ModelResult{}, retryError{}
			}
			return next(ctx, execution)
		}),
		agentgo.WithAfterRun(func(_ context.Context, run agentgo.AfterRunContext) error { runErr = run.Err; return nil }),
	)
	agent := agentgo.NewAgent(options...)
	original := fileArtifact{Path: "main.go", Content: "package main\nfunc main() {}"}
	if err := agent.PromptMessages(t.Context(), userFile(original), userFile(original)); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if beforeCalls != 1 || attempts != 3 || len(model.requests) != 2 {
		t.Fatalf("before=%d attempts=%d successful requests=%d", beforeCalls, attempts, len(model.requests))
	}
	for i, messages := range model.requests {
		if countContent(messages, original.Content) != 1 {
			t.Errorf("request %d did not expand source exactly once", i)
		}
	}
	report, ok := artifacts.GetArtifact("report.md")
	if !ok {
		t.Fatal("tool middleware did not register report")
	}
	if countContent(model.requests[1], report.(fileArtifact).Content) != 1 {
		t.Fatal("tool artifact absent from next prompt")
	}
	last := model.requests[1][len(model.requests[1])-1]
	if last.Role != agentgo.RoleTool || last.Metadata["tool_call_id"] != "report-1" {
		t.Fatalf("tool pairing lost: %+v", last)
	}
	snapshot := agent.Snapshot()
	for i := range 2 {
		source, ok := snapshot.State.Messages[i].Raw().(fileMessage)
		if !ok || source.File != original || snapshot.State.Messages[i].TextContent() != original.Content {
			t.Fatalf("message %d baseline changed", i)
		}
	}
	// A later Run invokes the hook again, but historical source cannot overwrite
	// an explicitly replaced artifact retained by the host.
	updated := fileArtifact{Path: original.Path, Content: "updated source"}
	if err := artifacts.AddArtifact(updated, true); err != nil {
		t.Fatal(err)
	}
	if err := agent.Prompt(t.Context(), "Review again."); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()
	if runErr != nil || beforeCalls != 2 {
		t.Fatalf("next run: before=%d err=%v", beforeCalls, runErr)
	}
	if countContent(model.requests[2], updated.Content) != 1 || countContent(model.requests[2], original.Content) != 0 {
		t.Fatal("next Run did not use replacement artifact")
	}
}

func countContent(messages []agentgo.Message, text string) int {
	count := 0
	for _, message := range messages {
		count += strings.Count(message.TextContent(), text)
	}
	return count
}

func TestArtifactTransformRebuildsRequestCoverage(t *testing.T) {
	artifacts := agentgo.NewArtifactManager()
	file := fileArtifact{Path: "a.go", Content: "original source"}
	if err := artifacts.AddArtifact(file, false); err != nil {
		t.Fatal(err)
	}
	transformer := fileTransformer(artifacts)
	input := []agentgo.AgentMessage{userFile(file), userFile(file)}
	view, err := transformer.Transform(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	again, err := transformer.Transform(t.Context(), view)
	if err != nil {
		t.Fatal(err)
	}
	for i := range view {
		if again[i].TextContent() != view[i].TextContent() || view[i].Raw().(fileMessage).File != file {
			t.Fatal("transform lost idempotence or Raw")
		}
	}
	// Simulate compaction removing the first occurrence: the surviving message
	// must contain the artifact again, even though the old view was a reference.
	surviving, err := transformer.Transform(t.Context(), view[1:])
	if err != nil || !strings.Contains(surviving[0].TextContent(), file.Content) {
		t.Fatalf("coverage was not rebuilt: %v", err)
	}
	artifacts.DeleteArtifact(file.ID())
	without, err := transformer.Transform(t.Context(), input)
	if err != nil || without[0].TextContent() != input[0].TextContent() {
		t.Fatal("missing artifact should leave source message unchanged")
	}
}

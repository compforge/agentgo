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
	model := &demoModel{}
	beforeCalls, attempts := 0, 0
	var runErr error
	options := append(artifactOptions(),
		agentgo.WithModel(model),
		agentgo.WithMaxRetries(1),
		agentgo.WithBeforeRun(func(ctx context.Context, run agentgo.BeforeRunContext) error {
			beforeCalls++
			if beforeCalls == 2 {
				if err := run.Artifacts.AddArtifact(fileArtifact{Path: "main.go", Content: "updated source"}, true); err != nil {
					return err
				}
			}
			return registerInitial(ctx, run)
		}),
		agentgo.WithModelMiddlewares(func(ctx context.Context, execution agentgo.ModelExecution, next agentgo.ModelExecuteFunc) (agentgo.ModelResult, error) {
			attempts++
			if _, ok := execution.Artifacts.GetArtifact("main.go"); !ok {
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
	var report fileArtifact
	for _, value := range agent.State().Artifacts {
		if value.ID() == "report.md" {
			report = value.(fileArtifact)
		}
	}
	if report.Path == "" {
		t.Fatal("tool middleware did not register report")
	}
	if countContent(model.requests[1], report.Content) != 1 {
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
	// an explicitly replaced artifact retained by the runtime.
	updated := fileArtifact{Path: original.Path, Content: "updated source"}
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
	// Exercise a reusable transformer using the capability supplied by AgentGo.
	agent := agentgo.NewAgent(agentgo.WithModel(&demoModel{}), agentgo.WithMaxTurns(1),
		agentgo.WithBeforeRun(func(ctx context.Context, run agentgo.BeforeRunContext) error {
			file := fileArtifact{Path: "a.go", Content: "original source"}
			if err := run.Artifacts.AddArtifact(file, false); err != nil {
				return err
			}
			transformer := fileTransformer()
			input := agentgo.TransformContext{Artifacts: run.Artifacts, Messages: []agentgo.AgentMessage{userFile(file), userFile(file)}}
			view, err := transformer.Transform(ctx, input)
			if err != nil {
				return err
			}
			again, err := transformer.Transform(ctx, agentgo.TransformContext{Artifacts: run.Artifacts, Messages: view})
			if err != nil {
				return err
			}
			for i := range view {
				if again[i].TextContent() != view[i].TextContent() || view[i].Raw().(fileMessage).File != file {
					t.Error("transform lost idempotence or Raw")
				}
			}
			surviving, err := transformer.Transform(ctx, agentgo.TransformContext{Artifacts: run.Artifacts, Messages: view[1:]})
			if err != nil {
				return err
			}
			if !strings.Contains(surviving[0].TextContent(), file.Content) {
				t.Error("coverage was not rebuilt")
			}
			run.Artifacts.DeleteArtifact(file.ID())
			without, err := transformer.Transform(ctx, input)
			if err != nil {
				return err
			}
			if without[0].TextContent() != input.Messages[0].TextContent() {
				t.Error("missing artifact should leave source unchanged")
			}
			return nil
		}))
	if err := agent.Prompt(t.Context(), "check"); err != nil {
		t.Fatal(err)
	}
	agent.WaitForIdle()
}

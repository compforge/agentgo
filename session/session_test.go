package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/codec"
)

func newTestSession(t *testing.T) (*Session, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "loop.jsonl")
	s, err := Create(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func mustRun(t *testing.T, s *Session) *Run {
	t.Helper()
	run, err := s.BeginRun()
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func recordsAt(t *testing.T, path string) []Record {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var records []Record
	result, err := Read(f, func(r Record) error { records = append(records, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.TruncatedTail {
		t.Fatal("unexpected torn tail")
	}
	return records
}

// +case:id=session_call_identity,desc=`Repeated model IDs in successive runs`,expect=`Distinct durable references and no overlapping recording scopes`
func TestRunIdentityAndConcurrentRecords(t *testing.T) {
	s, path := newTestSession(t)
	first := mustRun(t, s)
	if _, err := s.BeginRun(); err == nil {
		t.Fatal("overlapping run accepted")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := first.Record(agentgo.Event{Type: agentgo.EventToolInvokeStart, ToolID: "call"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := mustRun(t, s)
	execution := agentgo.Execution{ID: "model-1", Attempt: 1}
	if first.Ref(execution) == second.Ref(execution) {
		t.Fatal("references collide across runs")
	}
	if err := first.Record(agentgo.Event{Type: agentgo.EventAgentEnd}); err == nil {
		t.Fatal("closed run accepted event")
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	records := recordsAt(t, path)
	if len(records) != 37 {
		t.Fatalf("records=%d, want header + 4 boundaries + 32 events", len(records))
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	third := mustRun(t, reopened)
	if reopened.ID() != s.ID() || third.Ref(execution) == second.Ref(execution) {
		t.Fatal("reopen identity mismatch")
	}
}

func TestStreamUpdatesAndStateAreNotImplicitCheckpoints(t *testing.T) {
	s, path := newTestSession(t)
	run := mustRun(t, s)
	for i := 0; i < 100; i++ {
		if err := run.Record(agentgo.Event{Type: agentgo.EventMessageUpdate, Delta: "token"}); err != nil {
			t.Fatal(err)
		}
	}
	state := agentgo.AgentState{Messages: []agentgo.AgentMessage{agentgo.UserMsg("baseline")}}
	if err := run.Record(agentgo.Event{Type: agentgo.EventAgentEnd, State: &state}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LoadCheckpoint(); err != nil || ok {
		t.Fatalf("implicit checkpoint: %v %v", ok, err)
	}
	records := recordsAt(t, path)
	if len(records) != 3 {
		t.Fatalf("unexpected full snapshots/deltas: %d records", len(records))
	}
	if bytes.Contains(records[2].Data, []byte("baseline")) {
		t.Fatal("state leaked into observation")
	}
}

type testArtifact struct {
	Key  string `codec:"id"`
	Text string `codec:"text"`
}

func (a testArtifact) ID() string   { return a.Key }
func (a testArtifact) Kind() string { return "test" }

// +case:id=session_explicit_checkpoint,desc=`Call records arrive after an explicit checkpoint`,expect=`Load restores only checkpoint state, including typed artifacts and input queues`
func TestCheckpointIsExplicitAndTyped(t *testing.T) {
	c, err := agentgo.NewCodec(codec.Type[testArtifact]("test.artifact"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	s, err := Create(path, c)
	if err != nil {
		t.Fatal(err)
	}
	run := mustRun(t, s)
	snapshot := agentgo.AgentSnapshot{
		State: agentgo.AgentState{Messages: []agentgo.AgentMessage{agentgo.UserMsg("accepted")},
			Artifacts: []agentgo.Artifact{testArtifact{Key: "a", Text: "evidence"}},
			Progress:  agentgo.RunProgress{Active: true, NextTurn: true, CompletedTurns: 2}},
		SteeringQueue: []agentgo.AgentMessage{agentgo.UserMsg("steer")},
		FollowUpQueue: []agentgo.AgentMessage{agentgo.UserMsg("follow up")},
	}
	if err := run.SaveCheckpoint(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := run.Record(agentgo.Event{Type: agentgo.EventModelExecEnd, Message: agentgo.UserMsg("uncommitted"), Err: errors.New("provider failed")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(path, c)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	got, ok, err := restored.LoadCheckpoint()
	if err != nil || !ok {
		t.Fatalf("load: %v %v", ok, err)
	}
	if len(got.State.Messages) != 1 || got.State.Messages[0].TextContent() != "accepted" || got.State.Progress.CompletedTurns != 2 {
		t.Fatalf("restored state=%+v", got.State)
	}
	if got.State.Artifacts[0].(testArtifact).Text != "evidence" || got.SteeringQueue[0].TextContent() != "steer" || got.FollowUpQueue[0].TextContent() != "follow up" {
		t.Fatal("typed state or queues lost")
	}
}

// +case:id=session_torn_tail,desc=`Interrupted append followed by reopening`,expect=`Keep complete prefix and append with continuous sequence; reject complete corruption`
func TestReadAndOpenTornTail(t *testing.T) {
	for _, tail := range []string{`{"type":`, `{"type":"valid_but_unterminated"}`} {
		t.Run(tail, func(t *testing.T) {
			s, path := newTestSession(t)
			run := mustRun(t, s)
			if err := run.Record(agentgo.Event{Type: agentgo.EventAgentStart}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			original, _ := os.ReadFile(path)
			if err := os.WriteFile(path, append(original, []byte(tail)...), 0600); err != nil {
				t.Fatal(err)
			}
			f, _ := os.Open(path)
			result, err := Read(f, nil)
			f.Close()
			if err != nil || !result.TruncatedTail || result.Records != 3 {
				t.Fatalf("read=%+v %v", result, err)
			}
			reopened, err := Open(path, nil)
			if err != nil {
				t.Fatal(err)
			}
			next := mustRun(t, reopened)
			if next.ID() == run.ID() {
				t.Fatal("reused interrupted run ID")
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			if len(recordsAt(t, path)) != 4 {
				t.Fatal("tail not repaired")
			}
		})
	}
	s, path := newTestSession(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(path)
	corrupt := append(original, []byte("{invalid}\n")...)
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, nil); err == nil {
		t.Fatal("complete corruption accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(corrupt, after) {
		t.Fatal("corrupt file was mutated")
	}
}

func TestReadRejectsIdentityAndVersionCorruption(t *testing.T) {
	s, path := newTestSession(t)
	_ = mustRun(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(path)
	for _, change := range []func(string) string{
		func(s string) string { return strings.Replace(s, `"version":1`, `"version":999`, 1) },
		func(s string) string { return strings.Replace(s, `"seq":2`, `"seq":3`, 1) },
		func(s string) string {
			return strings.Replace(s, `"session_id":"`+sID(s)+`"`, `"session_id":"different"`, 1)
		},
	} {
		if _, err := Read(strings.NewReader(change(string(original))), nil); err == nil {
			t.Fatal("corrupt envelope accepted")
		}
	}
}

func sID(data string) string {
	var record Record
	_ = json.Unmarshal([]byte(strings.Split(data, "\n")[0]), &record)
	return record.SessionID
}

func TestRecordingFailureIsStickyAndReturnedOnClose(t *testing.T) {
	s, _ := newTestSession(t)
	run := mustRun(t, s)
	if err := s.file.Close(); err != nil {
		t.Fatal(err)
	}
	first := run.Record(agentgo.Event{Type: agentgo.EventAgentStart})
	if first == nil {
		t.Fatal("write failure hidden")
	}
	if err := run.Record(agentgo.Event{Type: agentgo.EventAgentEnd}); err != first {
		t.Fatalf("write resumed after failure: %v", err)
	}
	if err := s.Close(); !errors.Is(err, first) {
		t.Fatalf("close hid prior failure: %v", err)
	}
}

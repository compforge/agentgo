package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/compforge/agentgo"
	"github.com/compforge/agentgo/codec"
)

// Session owns a single append-only file. A session may contain sequential runs;
// concurrently independent loops must use different sessions. Methods serialize
// writes within this handle. The host owns exclusive file access across handles
// and processes, file placement, retention, and the decision to stop on an error.
//
// +spec=`Only explicit checkpoints are recovery state; call records never imply commit`
// +link=component://docs/session.md
type Session struct {
	mu         sync.Mutex
	file       *os.File
	codec      codec.Codec
	id         string
	seq        uint64
	active     *Run
	checkpoint json.RawMessage
	writeErr   error
	closed     bool
}

// Run is a recording scope for an AgentLoop invocation or standalone model work.
// It is independent of goroutine identity. Finish all producers before Close;
// closing never implies success.
type Run struct {
	session *Session
	id      string
}

// Create exclusively creates path with private permissions. A nil codec uses
// AgentGo's built-in types. Register application messages/artifacts and context
// wrappers in the supplied codec when they appear in events or checkpoints.
func Create(path string, c codec.Codec) (*Session, error) {
	c, err := resolveCodec(c)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	s := &Session{file: f, codec: c, id: id}
	data, _ := json.Marshal(struct {
		Version int `json:"version"`
	}{FormatVersion})
	if err := s.appendLocked(Record{Type: SessionStart, Data: data}, true); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return s, nil
}

// Open validates and reopens a session for append. It removes only an unfinished
// final line, never a corrupt complete line. A prior unclosed run stays visibly
// incomplete; BeginRun creates a fresh recording scope. No loop is resumed here.
func Open(path string, c codec.Codec) (*Session, error) {
	c, err := resolveCodec(c)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	s := &Session{file: f, codec: c}
	result, offset, err := read(f, func(record Record) error {
		if record.Type == Checkpoint {
			var snapshot agentgo.AgentSnapshot
			if err := c.Unmarshal(record.Data, &snapshot); err != nil {
				return fmt.Errorf("checkpoint %d: %w", record.Seq, err)
			}
			s.checkpoint = record.Data
		}
		return nil
	})
	if err == nil && result.TruncatedTail {
		err = f.Truncate(offset)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	s.id, s.seq = result.SessionID, result.Records
	return s, nil
}

func resolveCodec(c codec.Codec) (codec.Codec, error) {
	if c != nil {
		return c, nil
	}
	return agentgo.NewCodec()
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Session) ID() string { return s.id }
func (r *Run) ID() string     { return r.id }

// Ref returns the coordinate a timeline or evaluator can store without copying
// the corresponding request, response, or tool output.
func (r *Run) Ref(execution agentgo.Execution) Ref {
	return Ref{SessionID: r.session.id, RunID: r.id, ExecutionID: execution.ID, Attempt: execution.Attempt}
}

// BeginRun admits one recording scope, rejecting overlapping loops in a session.
func (s *Session) BeginRun() (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writableLocked(); err != nil {
		return nil, err
	}
	if s.active != nil {
		return nil, fmt.Errorf("session %s: run %s is still open", s.id, s.active.id)
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	r := &Run{session: s, id: id}
	if err := s.appendLocked(Record{Type: RunStart, RunID: id}, false); err != nil {
		return nil, err
	}
	s.active = r
	return r, nil
}

// Record appends a completed content or lifecycle observation. Token deltas,
// partial message snapshots, progress updates and the duplicate model_response
// notification are omitted unless it carries a length recovery decision. State
// is deliberately excluded: use SaveCheckpoint
// explicitly at a valid boundary. Errors are returned without changing the loop.
func (r *Run) Record(event agentgo.Event) error {
	s := r.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.writableLocked(); err != nil {
		return err
	}
	switch event.Type {
	case agentgo.EventModelStream, agentgo.EventMessageStart, agentgo.EventMessageUpdate, agentgo.EventToolExecUpdate:
		return nil
	case agentgo.EventModelResponse:
		if event.LengthRecovery == nil {
			return nil
		}
	}
	if event.Type == "" {
		return fmt.Errorf("session %s run %s: empty event type", s.id, r.id)
	}
	data := EventData{
		TurnIndex: event.TurnIndex, ModelRequest: event.ModelRequest,
		ModelAttempt: event.ModelAttempt, ModelCall: event.ModelCall, LengthRecovery: event.LengthRecovery,
		ToolID: event.ToolID, Tool: event.Tool, ToolLabel: event.ToolLabel, Args: event.Args,
		Disposition: event.Disposition, IsError: event.IsError,
		Summary: event.Summary, ContextOperation: event.ContextOperation,
		ContextItems: event.ContextItems, Compaction: event.Compaction,
	}
	if result := event.ToolResult; result != nil {
		data.ToolResult = &ToolResultData{ToolCallID: result.ToolCallID, Content: result.Content,
			ContentBlocks: result.ContentBlocks, IsError: result.IsError, Details: result.Details}
	}
	// A committed message has its own message_end record. turn_end only
	// describes the boundary, avoiding another copy of assistant/tool content.
	if event.Message != nil && (event.Type == agentgo.EventMessageEnd || event.Type == agentgo.EventModelExecEnd) {
		encoded, err := s.codec.Marshal(event.Message)
		if err != nil {
			return s.failLocked(fmt.Errorf("encode %s message: %w", event.Type, err))
		}
		if event.Type == agentgo.EventModelExecEnd && event.Err != nil {
			data.PartialMessage = encoded
		} else {
			data.Message = encoded
		}
	}
	if event.Err != nil {
		data.Error = event.Err.Error()
	}
	if event.RetryInfo != nil {
		data.Retry = &RetryData{MaxRetries: event.RetryInfo.MaxRetries, Delay: event.RetryInfo.Delay}
		if data.Error == "" && event.RetryInfo.Err != nil {
			data.Error = event.RetryInfo.Err.Error()
		}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return s.failLocked(fmt.Errorf("encode %s: %w", event.Type, err))
	}
	return s.appendLocked(Record{Type: string(event.Type), RunID: r.id, Timestamp: event.Timestamp, Execution: event.Execution, Data: encoded}, false)
}

// SaveCheckpoint appends and syncs a caller-selected Agent snapshot, including
// all preceding records. For a bare loop use AgentSnapshot{State: *event.State}
// at a committed boundary. Stateful Agents must supply their Snapshot to retain
// accepted steering/follow-up queues. No checkpoint is inferred from call output.
func (r *Run) SaveCheckpoint(snapshot agentgo.AgentSnapshot) error {
	s := r.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.writableLocked(); err != nil {
		return err
	}
	data, err := s.codec.Marshal(snapshot)
	if err != nil {
		return s.failLocked(fmt.Errorf("encode checkpoint: %w", err))
	}
	if err := s.appendLocked(Record{Type: Checkpoint, RunID: r.id, Data: data}, true); err != nil {
		return err
	}
	s.checkpoint = data
	return nil
}

// LoadCheckpoint returns the latest explicitly saved snapshot. The host rebinds
// model/tools/config and decides whether to install it. Records after this
// checkpoint remain observations; they are never automatically replayed.
func (s *Session) LoadCheckpoint() (agentgo.AgentSnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var snapshot agentgo.AgentSnapshot
	if len(s.checkpoint) == 0 {
		return snapshot, false, nil
	}
	err := s.codec.Unmarshal(s.checkpoint, &snapshot)
	return snapshot, err == nil, err
}

func (r *Run) writableLocked() error {
	if err := r.session.writableLocked(); err != nil {
		return err
	}
	if r.session.active != r {
		return fmt.Errorf("session %s: run %s is not open", r.session.id, r.id)
	}
	return nil
}

func (s *Session) writableLocked() error {
	if s.writeErr != nil {
		return s.writeErr
	}
	if s.closed {
		return fmt.Errorf("session %s: %w", s.id, os.ErrClosed)
	}
	return nil
}

func (s *Session) failLocked(err error) error {
	if s.writeErr == nil {
		s.writeErr = fmt.Errorf("session %s seq %d: %w", s.id, s.seq+1, err)
	}
	return s.writeErr
}

func (s *Session) appendLocked(record Record, sync bool) error {
	if err := s.writableLocked(); err != nil {
		return err
	}
	record.SessionID, record.Seq = s.id, s.seq+1
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	}
	data, err := json.Marshal(record)
	if err != nil {
		return s.failLocked(err)
	}
	data = append(data, '\n')
	n, err := s.file.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil && sync {
		err = s.file.Sync()
	}
	if err != nil {
		return s.failLocked(err)
	}
	s.seq++
	return nil
}

// Close ends this recording scope and syncs its records. The agent_end event,
// when present, carries the execution outcome; run_close is not a success flag.
func (r *Run) Close() error {
	s := r.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := r.writableLocked(); err != nil {
		return err
	}
	err := s.appendLocked(Record{Type: RunClose, RunID: r.id}, true)
	if err == nil {
		s.active = nil
	}
	return err
}

// Close syncs and releases the file, also returning any prior recording error.
// It does not fabricate an agent_end or close an unfinished run in the log.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.writeErr
	}
	err := errors.Join(s.writeErr, s.file.Sync(), s.file.Close())
	s.closed = true
	s.writeErr = err
	return err
}

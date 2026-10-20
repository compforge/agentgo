# Local agent sessions

`agentgo/session` records one logical agent conversation. A session may span
sequential loop runs; independently running loops use separate sessions. The
package owns JSONL recording and reading, not application scheduling, quality
judgments, retention, or a global timeline.

A typical analysis starts with an application-owned timeline, locates a loop,
then reads its session to inspect requests, responses, tool use, context changes,
and completion. No workflow recovery is required for this path.

## Identity and ownership

- A Session has a random, persistent `session_id`.
- `BeginRun` creates a fresh `run_id` for one loop invocation, including a later
  invocation after reopening the file. Only one run may be open per Session.
- `Execution.ID` remains the kernel's run-local logical call ID; `Attempt`
  distinguishes retries. `run.Ref(execution)` combines all four coordinates.
- `(session_id, seq)` identifies one JSONL record. Sequence is append order, not
  causality; event timestamps come from the source, not file write time.

The application chooses file paths and keeps the session-ID-to-path association.
An external timeline stores call references plus its own stage hierarchy, times
and status. Prompt and tool-output bodies stay in the Session. Neither package
must depend on the other's storage format.

One Session handle owns each file. Writes through that handle are serialized;
the host must prevent a second handle or process from opening the same file for
writing. The package does not implement distributed ownership or retention.

## Recording

```go
stateCodec, err := agentgo.NewCodec(agentcontext.CodecOptions()...)
if err != nil { return err }
s, err := session.Create(path, stateCodec)
if err != nil { return err }
defer func() { err = errors.Join(err, s.Close()) }() // named return err
run, err := s.BeginRun()
if err != nil { return err }

var recordErr error
for event := range agentgo.AgentLoop(ctx, input, agentContext, config) {
    if recordErr == nil {
        recordErr = run.Record(event)
    }
    if event.Execution != nil {
        ref := run.Ref(*event.Execution)
        // Supply ref and event to the application's timeline adapter.
        _ = ref
    }
    // Continue draining even if recording fails. The host may instead cancel
    // the loop, then drain it. A recording failure must remain visible.
}
return errors.Join(recordErr, run.Close())
```

For a stateful Agent, feed the same `run.Record(event)` from `Agent.Subscribe`.
Unsubscribe and close the recording scope after `WaitForIdle`, before recording
a subsequent run. Capture and report recording errors in the callback; they do
not alter the loop's execution result automatically.

The file records native event types with an `EventData` payload:

| Fact | Recorded content |
|---|---|
| `model_request` | Messages, tool declarations and resolved per-call Config after middleware, including summary and standalone calls |
| `model_exec_end` | Successful response in Message, or failed response evidence in PartialMessage alongside the error, including reported usage and provider state |
| `message_end` | Committed, codec-encoded AgentMessage, preserving application types |
| `tool_exec_start` | Requested tool and arguments |
| `tool_invoke_start` | Actual arguments after middleware and authorization |
| `tool_exec_end` | Complete result, rich content, details, error flag and disposition |
| Context, retry, turn and agent lifecycle | Native facts and summary without copying the full AgentState |

`model_request` describes the AgentGo executor boundary, not a provider's HTTP
wire body or effective provider defaults. It excludes CallOption functions and
credentials. Middleware short-circuits have a result without an executor request.
Message containers and mutable Config fields are detached for publication; opaque
message metadata values and tool declarations' schemas remain read-only. APIKey is
also removed from the in-memory request event. JSON-valued Config fields such as
ToolChoice and ResponseFormat.JSONSchema.Schema are frozen as json.RawMessage;
unencodable values carry a snapshot_error diagnostic without retaining the input.
`message_end` and `model_exec_end` have distinct meaning:
a model result is not proof that the corresponding conversation message committed.

Token deltas, partial message snapshots, tool progress/preview updates, and the
redundant `model_response` notification are not written. A `model_response` with a length recovery decision records that decision without duplicating the message content. Failed stream partial
content is recorded only once at the failed model boundary. A run does not
implicitly store a full snapshot at any lifecycle event.

The default codec knows built-in AgentGo messages. Supply a JSON-compatible codec
with custom message/artifact registrations, and `context.CodecOptions()` when
context-owned representations are used. Unknown typed messages cause an explicit
recording error rather than silently lowering away their original content.

## Standalone model calls

A recording run can also scope standalone calls. Use the same runtime and event
observer as nested model work; the host supplies IDs unique within that run.

```go
var recordErr error
ctx = agentgo.WithModelRuntime(ctx, middlewares, func(event agentgo.Event) {
    // This example is sequential. Synchronize error collection for concurrent calls.
    recordErr = errors.Join(recordErr, run.Record(event))
})
result, callErr := agentgo.GenerateModel(ctx, model, agentgo.ModelExecution{
    Execution: agentgo.Execution{
        ID: "classify", Kind: agentgo.ExecutionKindModel, Attempt: 1,
    },
    Request: agentgo.LLMRequest{
        Messages: messages,
        Config: &agentgo.CallConfig{ThinkingLevel: agentgo.ThinkingMedium},
    },
})
// result.Message may contain diagnostic content even when callErr != nil.
// Handle callErr and recordErr independently, then close the recording run.
```

The runtime emits start, the final request after middleware, routed attempts when
using a Router, and end. A middleware short-circuit omits the request and provider
attempts. GenerateModel adds no retries and does not commit messages. Partial
responses never create message_end or recovery checkpoints. Standalone completion
is determined by model_exec_end and its error; no agent_end is manufactured.

## Reading and interrupted files

`session.Read(reader, visit)` streams `Record` values without retaining the whole
file. Decode event `Data` into `session.EventData`, and decode its `Message` or
`PartialMessage` with the same codec registry. Match call records using Session,
Run, Execution ID and Attempt; use Execution.ParentID for nested work.

The first `session_start` record declares format version 1. Complete lines must
have contiguous sequence numbers and the same session identity. Unsupported
versions and complete corrupt lines fail. Only a final line without a newline is
ignored, even if that line happens to contain syntactically valid JSON.

`Open` validates the file and removes only that unfinished tail before appending.
An interrupted old run remains incomplete; a new run gets a fresh ID. `run_close`
means the recording scope was closed, not that execution succeeded. Success or
failure comes from `agent_end` for loops and `model_exec_end` for standalone calls.
A missing terminal event remains a gap. Kernel
event delivery is best-effort after cancellation, so continuously drain events.

`Record` writes directly to the file without an application buffer, but does not
fsync every event. `Run.Close`, `Session.Close`, and explicit checkpoints sync.
A write/encoding error is sticky and is returned on later writes and Close; the
host decides whether to cancel execution or continue with a reported evidence
gap. JSONL recording does not make tool side effects transactional.

## Optional checkpoints

Observation works without checkpoints. If a caller needs to save a local loop's
state, `run.SaveCheckpoint(snapshot)` appends a codec-encoded snapshot and syncs
it together with prior records. `s.LoadCheckpoint()` returns only the latest
explicit checkpoint; subsequent request/result records never advance it.

Choose checkpoints at valid committed boundaries, not on a timer. For a bare
loop, a completed `turn_end.State` can be wrapped in `AgentSnapshot{State: ...}`.
For a stateful Agent, capture `agent.Snapshot()` in a boundary listener to retain
accepted input queues. A terminal listener also includes AfterRun artifact writes.
Large checkpoints contain full history, so their cadence belongs to the caller.

Loading does not start a loop. Rebind model, tools and configuration before
installing the snapshot via the existing Agent/Loop APIs. No Review/workflow
state, automatic replay of tool effects, durable inbox or global resume protocol
is part of Session.

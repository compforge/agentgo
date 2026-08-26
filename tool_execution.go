package agentgo

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// executeToolCalls runs tool calls for one committed assistant turn using the
// shared concurrency and steering scheduler.
func executeToolCalls(ctx context.Context, turnIndex int, tools []Tool, calls []ToolCall, config LoopConfig, toolErrors map[string]int, sink eventSink) ([]ToolResult, []AgentMessage) {
	exec := newTurnToolExecutor(ctx, turnIndex, tools, config, toolErrors, sink)
	for _, call := range calls {
		exec.Add(call)
	}
	return exec.Wait()
}

// executeSingleToolCall wraps the complete validation, authorization and
// execution pipeline. A middleware can return a known ToolResult without
// invoking next, so replay never reaches gates or external side effects.
func executeSingleToolCall(ctx context.Context, tools []Tool, execution ToolExecution, config LoopConfig, failCount int, sink eventSink) ToolResult {
	call := execution.Call
	tool := findTool(tools, call.Name)
	label := toolLabel(tool)
	sink.emit(Event{
		Type:      EventToolExecStart,
		Execution: executionRef(execution.Execution),
		ToolID:    call.ID,
		Tool:      call.Name,
		ToolLabel: label,
		Args:      call.Args,
	})

	execute := func(ctx context.Context, execution ToolExecution) (ToolResult, error) {
		return executeToolCallCore(ctx, tool, execution, config, failCount, label, sink), nil
	}
	if len(config.ToolMiddlewares) > 0 {
		execute = buildToolMiddlewareChain(execute, config.ToolMiddlewares)
	}
	result, err := execute(ctx, execution)
	if err != nil {
		result = toolFailureResult(call, err.Error(), true)
	}
	// The assistant-issued ID is the durable pairing key. Middleware may
	// replace the outcome, but cannot retarget it to another tool call.
	result.ToolCallID = call.ID
	if !result.IsError && result.ToolName == "" {
		result.ToolName = call.Name
	}

	sink.emit(Event{
		Type:      EventToolExecEnd,
		Execution: executionRef(execution.Execution),
		ToolID:    call.ID,
		Tool:      call.Name,
		ToolLabel: label,
		Result:    result.Content,
		IsError:   result.IsError,
	})
	return result
}

// executeToolCallCore performs one real tool call without middleware or the
// start/end lifecycle events owned by executeSingleToolCall.
func executeToolCallCore(ctx context.Context, tool Tool, execution ToolExecution, config LoopConfig, failCount int, label string, sink eventSink) ToolResult {
	call := execution.Call
	if ctx.Err() != nil {
		return toolFailureResult(call, "Tool execution cancelled.", false)
	}
	if config.MaxToolErrors > 0 && failCount >= config.MaxToolErrors {
		return toolFailureResult(call,
			fmt.Sprintf("tool %q disabled after %d consecutive errors", call.Name, config.MaxToolErrors), false)
	}

	if tool == nil {
		return toolFailureResult(call, fmt.Sprintf("tool %q not found", call.Name), false)
	} else if err := validateToolArgs(tool, call); err != nil {
		return toolFailureResult(call, err.Error(), true)
	} else {
		// Stage 1: business-level input validation. Distinct from schema
		// validation above — Validators check semantics (write-before-read,
		// mtime drift, ...) using state the tool was constructed with.
		// Failures are surfaced as a normal tool_result with IsError=true so
		// the LLM can self-correct without prompting the user. Validators
		// MUST NOT prompt or mutate persistent state.
		if v, ok := tool.(Validator); ok {
			vr := v.Validate(ctx, call.Args)
			if !vr.OK {
				msg := vr.Message
				if msg == "" {
					msg = "tool input validation failed"
				}
				return toolFailureResult(call, msg, true)
			}
		}

		var preview json.RawMessage

		// Preview: if tool supports it, compute and emit preview before execution.
		// Preview runs only after args are validated so approval UIs can use it.
		if p, ok := tool.(Previewer); ok {
			data, err := p.Preview(ctx, call.Args)
			if err != nil {
				return toolFailureResult(call, fmt.Sprintf("preview tool %q: %v", call.Name, err), true)
			}
			preview = data
			sink.emit(Event{
				Type:       EventToolExecUpdate,
				Execution:  executionRef(execution.Execution),
				ToolID:     call.ID,
				Tool:       call.Name,
				ToolLabel:  label,
				Args:       call.Args,
				Result:     data,
				UpdateKind: ToolExecUpdatePreview,
			})
		}

		if config.ToolGate != nil {
			gateReq := GateRequest{
				Tool:      tool,
				Call:      call,
				ToolLabel: label,
				Preview:   preview,
			}
			decision, err := config.ToolGate(ctx, gateReq)
			if err != nil {
				decision = &GateDecision{Allowed: false, Reason: err.Error()}
			}
			if decision != nil && !decision.Allowed {
				reason := decision.Reason
				if reason == "" {
					reason = "tool execution denied"
				}
				return toolFailureResult(call, reason, false)
			}
			// Adopt the gate's rewrite before execution so the tool, progress
			// events, and middleware all see the approved arguments. The
			// assistant message keeps the model's original args — like the
			// exec-start event above, it records what was requested.
			if decision != nil && len(decision.UpdatedArgs) > 0 {
				call.Args = decision.UpdatedArgs
			}
		}

		// Inject progress callback so tools can report partial results
		progressCtx := WithToolProgress(ctx, func(progress ProgressPayload) {
			p := progress
			sink.emit(Event{
				Type:       EventToolExecUpdate,
				Execution:  executionRef(execution.Execution),
				ToolID:     call.ID,
				Tool:       call.Name,
				ToolLabel:  label,
				Args:       call.Args,
				Progress:   &p,
				UpdateKind: ToolExecUpdateProgress,
			})
		})

		var result ToolResult
		// ContentTool returns rich content blocks such as images.
		if ct, ok := tool.(ContentTool); ok {
			blocks, output, execErr := executeContentTool(progressCtx, ct, call)
			if execErr != nil {
				errContent, _ := json.Marshal(execErr.Error())
				result = ToolResult{
					ToolCallID: call.ID,
					Content:    errContent,
					IsError:    true,
				}
			} else {
				// When tool_reference blocks are present, keep them alongside
				// any text blocks in ContentBlocks. The provider layer splits
				// tool_reference into tool_result content and text into sibling
				// blocks within the same user message.
				refBlocks, siblingText := splitToolRefBlocks(blocks)
				if len(refBlocks) > 0 {
					resultBlocks := make([]ContentBlock, 0, len(refBlocks)+1)
					resultBlocks = append(resultBlocks, refBlocks...)
					if siblingText != "" {
						resultBlocks = append(resultBlocks, TextBlock(siblingText))
					}
					result = ToolResult{
						ToolCallID:    call.ID,
						Content:       pickContentSummary(output, blocks),
						ContentBlocks: resultBlocks,
					}
				} else {
					summary := pickContentSummary(output, blocks)
					result = ToolResult{
						ToolCallID:    call.ID,
						Content:       summary,
						ContentBlocks: blocks,
					}
				}
			}
		} else {
			output, execErr := tool.Execute(progressCtx, call.Args)
			if execErr != nil {
				errContent, _ := json.Marshal(execErr.Error())
				result = ToolResult{
					ToolCallID: call.ID,
					Content:    errContent,
					IsError:    true,
				}
			} else {
				result = ToolResult{
					ToolCallID: call.ID,
					Content:    output,
				}
			}
		}
		result.ToolName = call.Name
		return result
	}
}

func toolFailureResult(call ToolCall, msg string, countErr bool) ToolResult {
	content, _ := json.Marshal(msg)
	result := ToolResult{ToolCallID: call.ID, Content: content, IsError: true}
	if countErr {
		result.ToolName = call.Name
	}
	return result
}

// failToolCall emits the terminal tool_exec_end event and returns the error
// result for a call that failed before the tool ran. countErr sets ToolName
// on the result so the caller counts the failure toward toolErrors; denials,
// skips, and cancellations pass false (see executeSingleToolCall).
func failToolCall(sink eventSink, execution ToolExecution, label, msg string, countErr bool) ToolResult {
	call := execution.Call
	result := toolFailureResult(call, msg, countErr)
	sink.emit(Event{
		Type:      EventToolExecEnd,
		Execution: executionRef(execution.Execution),
		ToolID:    call.ID,
		Tool:      call.Name,
		ToolLabel: label,
		Result:    result.Content,
		IsError:   true,
	})
	return result
}

// skipToolCall creates a skipped result for an interrupted tool call.
func skipToolCall(execution ToolExecution, tools []Tool, sink eventSink) ToolResult {
	return skipToolCallWithMessage(execution, tools, sink, "Skipped due to queued user message.")
}

func skipToolCallWithMessage(execution ToolExecution, tools []Tool, sink eventSink, message string) ToolResult {
	call := execution.Call
	label := toolLabel(findTool(tools, call.Name))

	sink.emit(Event{
		Type:      EventToolExecStart,
		Execution: executionRef(execution.Execution),
		ToolID:    call.ID,
		Tool:      call.Name,
		ToolLabel: label,
		Args:      call.Args,
	})

	return failToolCall(sink, execution, label, message, false)
}

func newToolExecution(turnIndex int, call ToolCall) ToolExecution {
	return ToolExecution{
		Execution: Execution{
			ID:        call.ID,
			Kind:      ExecutionKindTool,
			TurnIndex: turnIndex,
			Attempt:   1,
		},
		Call: call,
	}
}

func findToolCall(calls []ToolCall, id string) ToolCall {
	for _, call := range calls {
		if call.ID == id {
			return call
		}
	}
	return ToolCall{ID: id}
}

func toolResultMessage(config LoopConfig, call ToolCall, result ToolResult) AgentMessage {
	if config.ToolResultMessageFactory != nil {
		if message := config.ToolResultMessageFactory(call, result); message != nil {
			return message
		}
	}
	return toolResultToMessage(result)
}

// toolResultToMessage converts a ToolResult into the default Message stored in
// context when the application has not installed a richer message factory.
func toolResultToMessage(tr ToolResult) Message {
	if len(tr.ContentBlocks) > 0 {
		return Message{
			Role:    RoleTool,
			Content: tr.ContentBlocks,
			Metadata: map[string]any{
				"tool_call_id": tr.ToolCallID,
				"tool_name":    tr.ToolName,
				"is_error":     tr.IsError,
			},
			Timestamp: time.Now(),
		}
	}
	msg := ToolResultMsg(tr.ToolCallID, tr.Content, tr.IsError)
	if tr.ToolName != "" {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]any)
		}
		msg.Metadata["tool_name"] = tr.ToolName
	}
	return msg
}

// stripToolCallBlocks removes ContentToolCall blocks from a content slice.
// Used when the model's output was truncated (StopReasonLength), where tool
// call arguments are likely incomplete / malformed JSON.
func stripToolCallBlocks(blocks []ContentBlock) []ContentBlock {
	filtered := blocks[:0:0]
	for _, b := range blocks {
		if b.Type != ContentToolCall {
			filtered = append(filtered, b)
		}
	}
	return filtered
}

// toolLabel returns the human-readable label for a tool.
func toolLabel(tool Tool) string {
	if tool == nil {
		return ""
	}
	if labeler, ok := tool.(ToolLabeler); ok {
		return labeler.Label()
	}
	return ""
}

// buildToolSpecs converts Tool interfaces to ToolSpec for the LLM.
// When a DeferFilter is present among the tools, unactivated deferred tools
// are excluded and activated deferred tools are sent with DeferLoading: true.
func buildToolSpecs(tools []Tool) []ToolSpec {
	if len(tools) == 0 {
		return nil
	}

	var filter DeferFilter
	for _, tool := range tools {
		if candidate, ok := tool.(DeferFilter); ok {
			filter = candidate
			break
		}
	}

	specs := make([]ToolSpec, 0, len(tools))
	for _, t := range tools {
		spec := ToolSpec{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  t.Schema(),
		}
		if filter != nil && filter.IsDeferred(t.Name()) {
			continue // unactivated deferred → don't send schema
		}
		if filter != nil && filter.WasDeferred(t.Name()) {
			spec.DeferLoading = true // activated deferred → send with defer_loading
		}
		if s, ok := t.(StrictSchemaTool); ok {
			strict := s.StrictSchema()
			spec.Strict = &strict
		}
		specs = append(specs, spec)
	}
	return specs
}

// buildToolMiddlewareChain wraps a complete tool call with the middleware stack.
// Outermost middleware is called first; innermost calls the actual tool.
func buildToolMiddlewareChain(exec ToolExecuteFunc, middlewares []ToolMiddleware) ToolExecuteFunc {
	for i := len(middlewares) - 1; i >= 0; i-- {
		mw := middlewares[i]
		next := exec
		exec = func(ctx context.Context, execution ToolExecution) (ToolResult, error) {
			return mw(ctx, execution, next)
		}
	}
	return exec
}

func executeContentTool(ctx context.Context, ct ContentTool, call ToolCall) ([]ContentBlock, json.RawMessage, error) {
	blocks, err := ct.ExecuteContent(ctx, call.Args)
	return blocks, contentBlocksTextSummary(blocks), err
}

func pickContentSummary(output json.RawMessage, blocks []ContentBlock) json.RawMessage {
	if len(output) > 0 {
		return output
	}
	return contentBlocksTextSummary(blocks)
}

func findTool(tools []Tool, name string) Tool {
	for _, t := range tools {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

// splitToolRefBlocks separates tool_reference blocks from text blocks.
// Returns the tool_reference blocks and concatenated text from text blocks.
// Used to format tool search results: tool_reference goes into tool_result
// content, text becomes a sibling block outside the tool_result.
func splitToolRefBlocks(blocks []ContentBlock) (refs []ContentBlock, text string) {
	var texts []string
	for _, b := range blocks {
		switch b.Type {
		case ContentToolRef:
			refs = append(refs, b)
		case ContentText:
			if b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
	}
	text = strings.Join(texts, "\n")
	return
}

// contentBlocksTextSummary extracts text from ContentBlocks as a JSON string
// for the Event.Result field. Returns nil if no text content.
func contentBlocksTextSummary(blocks []ContentBlock) json.RawMessage {
	var texts []string
	for _, b := range blocks {
		if b.Type == ContentText {
			texts = append(texts, b.Text)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	summary, _ := json.Marshal(strings.Join(texts, "\n"))
	return summary
}

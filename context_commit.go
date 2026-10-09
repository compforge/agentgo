package agentgo

import "context"

// Context preparation failures are not provider failures, even if a host wraps
// a retryable error. Retrying cannot repair a rejected durable state transition.
type contextPreparationError struct{ cause error }

func (e *contextPreparationError) Error() string { return "prepare context: " + e.cause.Error() }
func (e *contextPreparationError) Unwrap() error { return e.cause }

func stageContext(ctx context.Context, source *memoryArtifactManager) (context.Context, *memoryArtifactManager) {
	staged := newArtifactManager()
	if source != nil {
		for _, artifact := range source.ListArtifacts() {
			staged.artifacts[artifact.ID()] = artifact
		}
	}
	// Summary model middleware must use the same staged collection as compaction.
	runtime, ok := ctx.Value(modelExecutionRuntimeKey{}).(modelExecutionRuntime)
	if ok {
		runtime.artifacts = staged
		ctx = context.WithValue(ctx, modelExecutionRuntimeKey{}, runtime)
	}
	return ctx, staged
}

func acceptContext(agentCtx *AgentContext, config LoopConfig, result ContextCommitResult, staged *memoryArtifactManager, execution Execution, sink eventSink) error {
	if !result.Changed && !staged.changed {
		return nil
	}
	if !result.Changed {
		result.Messages = copyMessages(agentCtx.Messages)
	}
	result.Changed = true
	result.Artifacts = staged.ListArtifacts()
	if result.Compaction != nil {
		info := *result.Compaction
		info.Committed = false
		result.Compaction = &info
	}
	if config.CommitContext != nil {
		if err := config.CommitContext(result); err != nil {
			return &contextPreparationError{cause: err}
		}
	}
	agentCtx.Messages = copyMessages(result.Messages)
	if config.artifacts != nil {
		config.artifacts.replace(staged.artifacts)
	}
	config.ContextManager.Sync(agentCtx.Messages)
	if result.Compaction != nil {
		info := *result.Compaction
		info.Committed = true
		sink.emit(Event{Type: EventContextCompacted, Execution: executionRef(execution), Compaction: &info})
	}
	return nil
}

package agentgo

import (
	"context"
	"fmt"
	"time"
)

// Preparation is observed at the ContextManager boundary, not inferred from a
// successful compaction notification: projection can fail or leave history intact.
func prepareContext(ctx context.Context, manager ContextManager, execution Execution, input TransformContext, sink eventSink) (view []AgentMessage, compacted ContextCommitResult, err error) {
	sink.emit(Event{Type: EventContextPrepareStart, Execution: executionRef(execution), ContextOperation: ContextTransform})
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("transform context panicked: %v", recovered)
		}
		sink.emit(Event{Type: EventContextPrepareEnd, Execution: executionRef(execution), ContextOperation: ContextTransform, Err: err})
	}()
	view, err = manager.Transform(ctx, input)
	if err != nil {
		return
	}
	// The manager owns its threshold; the loop owns durable baseline submission.
	compacted, err = manager.Compact(ctx, input, CompactReasonThreshold)
	if err != nil {
		return
	}
	if compacted.Changed {
		view, err = manager.Transform(ctx, TransformContext{Messages: compacted.Messages, Artifacts: input.Artifacts})
	}
	return
}

// transformContextView repeats only the cheap view stage after late steering.
func transformContextView(ctx context.Context, manager ContextManager, input TransformContext) (view []AgentMessage, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("transform context panicked: %v", recovered)
		}
	}()
	return manager.Transform(ctx, input)
}

func recoverContext(ctx context.Context, manager ContextManager, execution Execution, input TransformContext, cause error, sink eventSink) (recovery ContextRecoveryResult, err error) {
	sink.emit(Event{Type: EventContextPrepareStart, Execution: executionRef(execution), ContextOperation: ContextRecoverOverflow})
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("recover context panicked: %v", recovered)
		}
		sink.emit(Event{Type: EventContextPrepareEnd, Execution: executionRef(execution), ContextOperation: ContextRecoverOverflow, Err: err})
	}()
	return manager.RecoverOverflow(ctx, input, cause)
}

func waitForRetry(ctx context.Context, execution Execution, delay time.Duration, sink eventSink) (err error) {
	sink.emit(Event{Type: EventRetryWaitStart, Execution: executionRef(execution), RetryInfo: &RetryInfo{Delay: delay}})
	defer func() {
		sink.emit(Event{Type: EventRetryWaitEnd, Execution: executionRef(execution), Err: err})
	}()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

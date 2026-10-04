package agentgo

import (
	"context"
	"fmt"
	"time"
)

// Preparation is observed at the ContextManager boundary, not inferred from a
// successful compaction notification: projection can fail or leave history intact.
func projectContext(ctx context.Context, manager ContextManager, execution Execution, messages []AgentMessage, sink eventSink) (projection ContextProjection, err error) {
	sink.emit(Event{Type: EventContextPrepareStart, Execution: executionRef(execution), ContextOperation: ContextProject})
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("project context panicked: %v", recovered)
		}
		sink.emit(Event{Type: EventContextPrepareEnd, Execution: executionRef(execution), ContextOperation: ContextProject, Err: err})
	}()
	return manager.Project(ctx, messages)
}

func recoverContext(ctx context.Context, manager ContextManager, execution Execution, messages []AgentMessage, cause error, sink eventSink) (recovery ContextRecoveryResult, err error) {
	sink.emit(Event{Type: EventContextPrepareStart, Execution: executionRef(execution), ContextOperation: ContextRecoverOverflow})
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("recover context panicked: %v", recovered)
		}
		sink.emit(Event{Type: EventContextPrepareEnd, Execution: executionRef(execution), ContextOperation: ContextRecoverOverflow, Err: err})
	}()
	return manager.RecoverOverflow(ctx, messages, cause)
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

package agentgo

import (
	"context"
	"fmt"
	"time"
)

// compactContext prepares a candidate; only acceptContext may publish it.
func compactContext(ctx context.Context, manager ContextManager, execution Execution, input TransformContext, sink eventSink) (result ContextCommitResult, err error) {
	sink.emit(Event{Type: EventContextPrepareStart, Execution: executionRef(execution), ContextOperation: ContextCompact})
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("compact context panicked: %v", recovered)
		}
		sink.emit(Event{Type: EventContextPrepareEnd, Execution: executionRef(execution), ContextOperation: ContextCompact, Err: err})
	}()
	return manager.Compact(ctx, input, CompactReasonThreshold)
}

// transformContextView runs once against the accepted baseline for this request.
func transformContextView(ctx context.Context, manager ContextManager, execution Execution, input TransformContext, sink eventSink) (view []AgentMessage, err error) {
	sink.emit(Event{Type: EventContextPrepareStart, Execution: executionRef(execution), ContextOperation: ContextTransform})
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("transform context panicked: %v", recovered)
		}
		sink.emit(Event{Type: EventContextPrepareEnd, Execution: executionRef(execution), ContextOperation: ContextTransform, Err: err})
	}()
	return manager.Transform(ContextWithExecution(ctx, execution), input)
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

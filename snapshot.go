package agentgo

import (
	"context"
	"fmt"
)

// Snapshot returns the stateful Agent's state and accepted input queues from
// one critical section. Its portable message slices are safe for the caller to
// encode or retain. Artifact slices are copied; their application-owned payloads
// are shared, so mutate them through replacement or application synchronization.
func (a *Agent) Snapshot() AgentSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.snapshotLocked()
}

func (a *Agent) snapshotLocked() AgentSnapshot {
	return AgentSnapshot{
		State:         a.stateLocked(),
		SteeringQueue: copyMessages(a.steeringQ),
		FollowUpQueue: copyMessages(a.followUpQ),
	}
}

// SetSnapshot directly replaces the portable state and accepted input queues
// of an idle Agent. Normal recovery may use WithSnapshotLoader so
// Continue can restore it automatically. Hold the lifecycle first with
// HoldRuns when replacing a snapshot around live runs.
func (a *Agent) SetSnapshot(snapshot AgentSnapshot) error {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.isRunning {
		return fmt.Errorf("cannot set snapshot: %w", ErrAlreadyRunning)
	}
	values, err := artifactValues(snapshot.State.Artifacts)
	if err != nil {
		return fmt.Errorf("restore artifacts: %w", err)
	}
	a.artifacts.replace(values)
	a.applySnapshotLocked(snapshot)
	return nil
}

// prepareRun must run with runMu held. Storage I/O and initialization run
// without a.mu; observers keep seeing the accepted baseline until the prepared
// messages, queues and material collection can be installed together.
func (a *Agent) prepareRun(ctx context.Context, kind RunKind, input []AgentMessage) error {
	a.mu.Lock()
	loader, hook := a.snapshotLoader, a.beforeRun
	if loader == nil && hook == nil {
		a.mu.Unlock()
		return nil
	}
	snapshot := a.snapshotLocked()
	a.mu.Unlock()

	if loader != nil {
		loaded, err := callSnapshotLoader(ctx, loader, SnapshotLoadContext{Kind: kind, Snapshot: snapshot})
		if err != nil {
			return fmt.Errorf("load snapshot: %w", err)
		}
		snapshot = loaded
	}
	values, err := artifactValues(snapshot.State.Artifacts)
	if err != nil {
		return fmt.Errorf("restore artifacts: %w", err)
	}
	// Only the staged manager is exposed to initialization. There is no replay
	// of operations against a different baseline, and rejected CRUD never leaks
	// through State or Snapshot. Payloads keep their documented shared semantics.
	prepared := newArtifactManager()
	prepared.replace(values)
	if hook != nil {
		err := callBeforeRun(ctx, hook, BeforeRunContext{
			Artifacts: prepared,
			Kind:      kind,
			Snapshot:  snapshot,
			Input:     copyMessages(input),
		})
		if err != nil {
			return fmt.Errorf("before run: %w", err)
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held > 0 {
		return ErrRunsHeld
	}
	if a.isRunning {
		return ErrAlreadyRunning
	}
	a.artifacts = prepared
	a.applySnapshotLocked(snapshot)
	return nil
}

func callSnapshotLoader(ctx context.Context, loader SnapshotLoader, run SnapshotLoadContext) (snapshot AgentSnapshot, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return loader(ctx, run)
}

func callBeforeRun(ctx context.Context, hook BeforeRunHook, run BeforeRunContext) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	return hook(ctx, run)
}

func (a *Agent) applySnapshotLocked(snapshot AgentSnapshot) {
	a.messages = copyMessages(snapshot.State.Messages)
	a.totalUsage = snapshot.State.TotalUsage
	a.runProgress = cloneRunProgress(snapshot.State.Progress)
	a.steeringQ = copyMessages(snapshot.SteeringQueue)
	a.followUpQ = copyMessages(snapshot.FollowUpQueue)

	// In-flight projections are process-local. A restored snapshot always
	// starts from a stable boundary and rebinds these values on the next run.
	a.streamMessage = nil
	a.pendingToolCalls = make(map[string]struct{})
	a.lastError = ""
	a.skipNextInitialSteeringPoll = false
	a.wantAbortMarker.Store(false)
	a.syncContextManagerLocked()
}

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
// of an idle Agent. Normal recovery should return a snapshot from BeforeRun so
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

// prepareRun must run with runMu held. BeforeRun runs without a.mu so adapters
// may perform storage I/O without blocking state observation; runMu keeps the
// admission snapshot stable until the returned baseline is installed.
func (a *Agent) prepareRun(ctx context.Context, kind RunKind, input []AgentMessage) error {
	a.mu.Lock()
	hook := a.beforeRun
	if hook == nil {
		a.mu.Unlock()
		return nil
	}
	before := a.artifacts.beginAdmission()
	accepted := false
	defer func() {
		if !accepted {
			a.artifacts.replace(before)
		}
	}()
	run := BeforeRunContext{
		Artifacts: a.artifacts,
		Kind:      kind,
		Snapshot:  a.snapshotLocked(),
		Input:     copyMessages(input),
	}
	a.mu.Unlock()

	snapshot, err := callBeforeRun(ctx, hook, run)
	if err != nil {
		return fmt.Errorf("before run: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.held > 0 {
		return ErrRunsHeld
	}
	if a.isRunning {
		return ErrAlreadyRunning
	}
	values, err := artifactValues(snapshot.State.Artifacts)
	if err != nil {
		return fmt.Errorf("restore artifacts: %w", err)
	}
	a.artifacts.finishAdmission(values)
	accepted = true
	a.applySnapshotLocked(snapshot)
	return nil
}

func callBeforeRun(ctx context.Context, hook BeforeRunHook, run BeforeRunContext) (snapshot AgentSnapshot, err error) {
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

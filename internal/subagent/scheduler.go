package subagent

import (
	"context"
	"time"
)

func (manager *Manager) Cancel(ctx context.Context, id ID) error {
	if manager == nil {
		return managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	if err := validCallContext(ctx); err != nil {
		return err
	}
	if !manager.validID(id) {
		return manager.error(ErrInvalidTask, "subagent task identity is invalid", true)
	}
	manager.mu.Lock()
	record := manager.tasks[id]
	if record == nil {
		err := manager.missingTaskErrorLocked(id)
		manager.mu.Unlock()
		return err
	}
	if record.completion != nil || record.settlementStarted {
		manager.mu.Unlock()
		return manager.error(ErrTaskTerminal, "subagent task is terminal or settling", true)
	}
	result := manager.cancellationRunResultLocked(record, StopCancelled)
	if record.snapshot.Status == StatusQueued {
		manager.removeQueuedLocked(record)
		record.cancel(manager.error(ErrCancelled, "subagent task was cancelled", true))
		if !manager.beginSettlementLocked(record, result) {
			manager.mu.Unlock()
			return manager.error(ErrInvalidTransition, "subagent cancellation settlement failed", false)
		}
		manager.runWG.Add(1)
		manager.mu.Unlock()
		manager.settleRecord(record, result, false)
		manager.runWG.Done()
		return nil
	}
	if record.forcedRunResult == nil {
		record.forcedRunResult = cloneRunResultPointer(result)
	}
	record.cancel(manager.error(ErrCancelled, "subagent task was cancelled", true))
	manager.mu.Unlock()
	return nil
}

func (manager *Manager) MoveToBackground(ctx context.Context, id ID) error {
	if manager == nil {
		return managerStaticError(ErrShutdown, "subagent manager is unavailable", false)
	}
	if err := validCallContext(ctx); err != nil {
		return err
	}
	if !manager.validID(id) {
		return manager.error(ErrInvalidTask, "subagent task identity is invalid", true)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	record := manager.tasks[id]
	if record == nil {
		return manager.missingTaskErrorLocked(id)
	}
	return manager.detachLocked(record, "manual")
}

// detachLocked contains the placement linearization point. The runtime switch
// and parent bridge detach must succeed before the observable task placement
// changes or a foreground waiter is released.
func (manager *Manager) detachLocked(record *managedTask, reason string) error {
	if record.completion != nil || record.settlementStarted || record.forcedRunResult != nil || IsTerminal(record.snapshot.Status) {
		return manager.error(ErrTaskTerminal, "subagent task is terminal", true)
	}
	if record.snapshot.Placement == Background {
		return nil
	}
	controller, ok := record.prepared.(PreparedPlacementController)
	if !ok {
		return manager.error(ErrInvalidTransition, "subagent placement controller is unavailable", false)
	}
	changed, panicked := callPlacementController(controller)
	if panicked || !changed {
		return manager.error(ErrInvalidTransition, "subagent runtime could not move to background", false)
	}
	from := record.snapshot.Placement
	record.snapshot.Placement = Background
	change := &PlacementChange{From: from, To: Background, Reason: reason}
	published, err := manager.hub.Publish(Event{
		TaskID: record.snapshot.ID, Kind: EventPlacementChanged, Snapshot: snapshotPointer(record.snapshot), Placement: change,
	})
	if err != nil {
		// The runtime already crossed its irreversible linearization point. Keep
		// the matching local state even when the event log is exhausted.
		manager.signalForegroundLocked(record, true)
		return manager.externalError(err, ErrInternal, "subagent placement event failed", false)
	}
	record.snapshot.Revision = published.Revision
	manager.signalForegroundLocked(record, true)
	return nil
}

func (manager *Manager) dispatchLocked() {
	if !manager.accepting {
		return
	}
	for manager.running < manager.limits.MaxConcurrent && len(manager.queue) > 0 {
		record := manager.queue[0]
		copy(manager.queue, manager.queue[1:])
		manager.queue[len(manager.queue)-1] = nil
		manager.queue = manager.queue[:len(manager.queue)-1]
		if record == nil || record.completion != nil || record.snapshot.Status != StatusQueued {
			continue
		}
		if !manager.reduceRunningLocked(record) {
			record.cancel(manager.error(ErrInternal, "subagent scheduler failed", false))
			result := manager.cancellationRunResultLocked(record, StopCancelled)
			if manager.beginSettlementLocked(record, result) {
				manager.runWG.Add(1)
				go func() {
					defer manager.runWG.Done()
					manager.settleRecord(record, result, false)
				}()
			}
			continue
		}
		record.runningSlot = true
		manager.running++
		manager.runWG.Add(1)
		if manager.limits.MaxTaskDuration > 0 {
			id := record.snapshot.ID
			record.durationTimer = time.AfterFunc(manager.limits.MaxTaskDuration, func() {
				manager.timeoutTask(id)
			})
		}
		go manager.runTask(record)
	}
}

func (manager *Manager) reduceRunningLocked(record *managedTask) bool {
	if !CanTransition(record.snapshot.Status, StatusRunning) {
		return false
	}
	previous := record.snapshot.Clone()
	startedAt := manager.clock()
	record.snapshot.Status = StatusRunning
	record.snapshot.StartedAt = &startedAt
	if _, err := manager.publishSnapshotLocked(record, EventRunning); err != nil {
		record.snapshot = previous
		return false
	}
	return true
}

func (manager *Manager) runTask(record *managedTask) {
	defer manager.runWG.Done()
	result, runPanicked := runPreparedTask(record.prepared, record.ctx, func(event AgentEvent) error {
		return manager.reduceAgentEvent(record.snapshot.ID, event)
	})

	manager.mu.Lock()
	if record.completion == nil {
		if record.forcedRunResult != nil {
			result = record.forcedRunResult.Clone()
		}
		_ = manager.beginSettlementLocked(record, result)
	}
	manager.mu.Unlock()

	manager.settleRecord(record, result, runPanicked)
}

func (manager *Manager) beginSettlementLocked(record *managedTask, result RunResult) bool {
	if record == nil || record.completion != nil || record.settlementStarted || !CanTransition(record.snapshot.Status, StatusSettling) {
		return false
	}
	record.snapshot.Status = StatusSettling
	record.settlementStarted = true
	record.forcedRunResult = cloneRunResultPointer(result)
	// Settlement owns external resources after this point. An exhausted event
	// log must not roll ownership back or authorize a second Settle call.
	_, _ = manager.publishSnapshotLocked(record, EventSettling)
	return true
}

func (manager *Manager) settleRecord(record *managedTask, result RunResult, earlierPanicked bool) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), manager.shutdownTimeout)
	candidate, settlePanicked := settlePreparedTask(record.prepared, cleanupCtx, result)
	cancel()
	manager.mu.Lock()
	manager.completeRunnerLocked(record, candidate, earlierPanicked || settlePanicked)
	if record.durationTimer != nil {
		record.durationTimer.Stop()
	}
	if record.runningSlot {
		record.runningSlot = false
		manager.running--
	}
	manager.dispatchLocked()
	manager.mu.Unlock()
}

func (manager *Manager) timeoutTask(id ID) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	record := manager.tasks[id]
	if record == nil || record.completion != nil || !record.runningSlot {
		return
	}
	if record.snapshot.Status != StatusRunning && record.snapshot.Status != StatusWaitingConfirmation {
		return
	}
	result := manager.timeoutRunResultLocked(record)
	if record.forcedRunResult == nil && !record.settlementStarted {
		record.forcedRunResult = cloneRunResultPointer(result)
	}
	record.cancel(manager.error(ErrTimedOut, "subagent task timed out", true))
}

func (manager *Manager) cancellationRunResultLocked(record *managedTask, reason StopReason) RunResult {
	return runResultFromManagerCompletion(manager.cancellationCompletionLocked(record, reason))
}

func (manager *Manager) timeoutRunResultLocked(record *managedTask) RunResult {
	return runResultFromManagerCompletion(manager.timeoutCompletionLocked(record))
}

func runResultFromManagerCompletion(completion Completion) RunResult {
	return RunResult{
		Status: completion.Status, StopReason: completion.StopReason, Summary: completion.Summary,
		Usage: completion.Usage, Error: cloneSafeError(completion.Error),
	}
}

func cloneRunResultPointer(result RunResult) *RunResult {
	cloned := result.Clone()
	return &cloned
}

func (manager *Manager) removeQueuedLocked(wanted *managedTask) {
	for index, record := range manager.queue {
		if record != wanted {
			continue
		}
		copy(manager.queue[index:], manager.queue[index+1:])
		manager.queue[len(manager.queue)-1] = nil
		manager.queue = manager.queue[:len(manager.queue)-1]
		return
	}
}

func (manager *Manager) watchParentCancellation(id ID, parent context.Context) {
	if parent == nil {
		return
	}
	manager.mu.Lock()
	record := manager.tasks[id]
	if record == nil {
		manager.mu.Unlock()
		return
	}
	foregroundDone := record.foregroundDone
	manager.mu.Unlock()

	select {
	case <-foregroundDone:
		return
	case <-manager.lifecycleCtx.Done():
		return
	case <-parent.Done():
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	record = manager.tasks[id]
	if record == nil || record.completion != nil || record.snapshot.Placement != Foreground {
		return
	}
	if record.snapshot.Status == StatusQueued {
		manager.removeQueuedLocked(record)
	}
	if record.settlementStarted {
		return
	}
	result := manager.cancellationRunResultLocked(record, StopCancelled)
	if record.snapshot.Status == StatusQueued {
		record.cancel(manager.error(ErrCancelled, "subagent parent execution was cancelled", true))
		if manager.beginSettlementLocked(record, result) {
			manager.runWG.Add(1)
			go func() {
				defer manager.runWG.Done()
				manager.settleRecord(record, result, false)
			}()
		}
		return
	}
	if record.forcedRunResult == nil {
		record.forcedRunResult = cloneRunResultPointer(result)
	}
	record.cancel(manager.error(ErrCancelled, "subagent parent execution was cancelled", true))
}

func (manager *Manager) observeFirstProviderRequest(id ID, requestCtx context.Context) {
	if requestCtx == nil {
		return
	}
	manager.mu.Lock()
	record := manager.tasks[id]
	if record == nil || record.firstRequestObserved || record.completion != nil || record.snapshot.Placement != Foreground {
		manager.mu.Unlock()
		return
	}
	record.firstRequestObserved = true
	done := record.done
	duration := manager.limits.AutoBackgroundAfter
	manager.mu.Unlock()

	go func() {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-requestCtx.Done():
			return
		case <-done:
			return
		case <-manager.lifecycleCtx.Done():
			return
		case <-timer.C:
		}
		manager.mu.Lock()
		defer manager.mu.Unlock()
		record := manager.tasks[id]
		if record == nil || record.completion != nil || record.snapshot.Placement != Foreground {
			return
		}
		_ = manager.detachLocked(record, "automatic_timeout")
	}()
}

func (manager *Manager) Shutdown(ctx context.Context) error {
	if manager == nil {
		return nil
	}
	if err := validCallContext(ctx); err != nil {
		return err
	}
	manager.shutdownOnce.Do(func() {
		manager.mu.Lock()
		manager.accepting = false
		manager.shuttingDown = true
		manager.queue = nil
		for _, record := range manager.tasks {
			if record.completion != nil || record.settlementStarted {
				continue
			}
			result := manager.cancellationRunResultLocked(record, StopApplicationClosed)
			record.cancel(manager.error(ErrCancelled, "subagent manager is shutting down", true))
			if record.snapshot.Status == StatusQueued {
				if manager.beginSettlementLocked(record, result) {
					manager.runWG.Add(1)
					go func(record *managedTask, result RunResult) {
						defer manager.runWG.Done()
						manager.settleRecord(record, result, false)
					}(record, result)
				}
			} else if record.forcedRunResult == nil {
				record.forcedRunResult = cloneRunResultPointer(result)
			}
		}
		manager.lifecycleCancel(manager.error(ErrShutdown, "subagent manager is shutting down", false))
		manager.mu.Unlock()

		go func() {
			// A Submit may already be inside Reserve/Prepare when admission is
			// stopped. Let that transaction observe accepting=false and release
			// its reservation before declaring shutdown complete.
			manager.admissionMu.Lock()
			manager.admissionMu.Unlock()
			manager.runWG.Wait()
			manager.hub.Close()
			close(manager.shutdownDone)
		}()
	})

	timer := time.NewTimer(manager.shutdownTimeout)
	defer timer.Stop()
	select {
	case <-manager.shutdownDone:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return manager.error(ErrTimedOut, "subagent shutdown exceeded its time limit", true)
	}
}

func runPreparedTask(prepared PreparedTask, ctx context.Context, sink EventSink) (result RunResult, panicked bool) {
	defer func() {
		if recover() != nil {
			result = RunResult{}
			panicked = true
		}
	}()
	return prepared.Run(ctx, sink), false
}

func settlePreparedTask(prepared PreparedTask, ctx context.Context, result RunResult) (completion Completion, panicked bool) {
	defer func() {
		if recover() != nil {
			completion = Completion{}
			panicked = true
		}
	}()
	return prepared.Settle(ctx, result), false
}

func callPlacementController(controller PreparedPlacementController) (changed bool, panicked bool) {
	defer func() {
		if recover() != nil {
			changed = false
			panicked = true
		}
	}()
	return controller.MoveToBackground(), false
}

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
	defer manager.mu.Unlock()
	record := manager.tasks[id]
	if record == nil {
		return manager.missingTaskErrorLocked(id)
	}
	if record.completion != nil {
		return manager.error(ErrTaskTerminal, "subagent task is terminal", true)
	}
	if record.snapshot.Status == StatusQueued {
		manager.removeQueuedLocked(record)
	}
	record.cancel(manager.error(ErrCancelled, "subagent task was cancelled", true))
	manager.completeLocked(record, manager.cancellationCompletionLocked(record, StopCancelled))
	manager.dispatchLocked()
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
	if record.completion != nil || IsTerminal(record.snapshot.Status) {
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
			manager.completeLocked(record, manager.cancellationCompletionLocked(record, StopCancelled))
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
	candidate, panicked := runPreparedTask(record.prepared, record.ctx, func(event AgentEvent) error {
		return manager.reduceAgentEvent(record.snapshot.ID, event)
	})

	manager.mu.Lock()
	manager.completeRunnerLocked(record, candidate, panicked)
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
	record.cancel(manager.error(ErrTimedOut, "subagent task timed out", true))
	manager.completeLocked(record, manager.timeoutCompletionLocked(record))
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
	record.cancel(manager.error(ErrCancelled, "subagent parent execution was cancelled", true))
	manager.completeLocked(record, manager.cancellationCompletionLocked(record, StopCancelled))
	manager.dispatchLocked()
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
			if record.completion != nil {
				continue
			}
			record.cancel(manager.error(ErrCancelled, "subagent manager is shutting down", true))
			manager.completeLocked(record, manager.cancellationCompletionLocked(record, StopApplicationClosed))
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

func runPreparedTask(prepared PreparedTask, ctx context.Context, sink EventSink) (completion Completion, panicked bool) {
	defer func() {
		if recover() != nil {
			completion = Completion{}
			panicked = true
		}
	}()
	return prepared.Run(ctx, sink), false
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

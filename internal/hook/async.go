package hook

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type asyncJob struct {
	ctx      context.Context
	cancel   context.CancelFunc
	started  time.Time
	run      func(context.Context) actionOutcome
	complete func(actionOutcome)
}

type asyncPool struct {
	mu          sync.RWMutex
	accepting   bool
	queue       chan asyncJob
	root        context.Context
	cancel      context.CancelFunc
	workers     sync.WaitGroup
	workersDone chan struct{}
	stopOnce    sync.Once
	closeOnce   sync.Once
	outstanding atomic.Int64
	closeResult int
	drainGrace  time.Duration
	joinGrace   time.Duration
}

func newAsyncPool(workerCount, queueSize int, drainGrace, joinGrace time.Duration) *asyncPool {
	if workerCount <= 0 {
		workerCount = 4
	}
	if queueSize <= 0 {
		queueSize = 64
	}
	if drainGrace <= 0 {
		drainGrace = 2 * time.Second
	}
	if joinGrace <= 0 {
		joinGrace = time.Second
	}
	root, cancel := context.WithCancel(context.Background())
	p := &asyncPool{accepting: true, queue: make(chan asyncJob, queueSize), root: root, cancel: cancel, workersDone: make(chan struct{}), drainGrace: drainGrace, joinGrace: joinGrace}
	for i := 0; i < workerCount; i++ {
		p.workers.Add(1)
		go p.worker()
	}
	return p
}

func (p *asyncPool) enqueue(timeout time.Duration, run func(context.Context) actionOutcome, complete func(actionOutcome)) bool {
	if p == nil {
		return false
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(p.root, timeout)
	} else {
		ctx, cancel = context.WithCancel(p.root)
	}
	job := asyncJob{ctx: ctx, cancel: cancel, started: time.Now(), run: run, complete: complete}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.accepting {
		cancel()
		return false
	}
	p.outstanding.Add(1)
	select {
	case p.queue <- job:
		return true
	default:
		p.outstanding.Add(-1)
		cancel()
		return false
	}
}

func (p *asyncPool) worker() {
	defer p.workers.Done()
	for job := range p.queue {
		p.execute(job)
	}
}

func (p *asyncPool) execute(job asyncJob) {
	defer p.outstanding.Add(-1)
	outcome := actionOutcome{}
	func() {
		defer func() {
			if recover() != nil {
				outcome = actionOutcome{code: DiagnosticActionPanic, stage: "run", err: errActionPanic}
			}
		}()
		if err := job.ctx.Err(); err != nil {
			outcome = actionOutcome{code: DiagnosticActionTimeout, stage: "queue", err: err}
		} else {
			outcome = job.run(job.ctx)
		}
	}()
	job.cancel()
	if job.complete != nil {
		outcome.duration = time.Since(job.started)
		job.complete(outcome)
	}
}

func (p *asyncPool) close() int {
	if p == nil {
		return 0
	}
	p.closeOnce.Do(func() {
		p.beginClose()
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), p.drainGrace)
		if !p.waitWorkers(drainCtx) {
			p.closeResult = int(p.outstanding.Load())
			if p.closeResult < 0 {
				p.closeResult = 0
			}
			p.cancelWorkers()
			joinCtx, cancelJoin := context.WithTimeout(context.Background(), p.joinGrace)
			p.waitWorkers(joinCtx)
			cancelJoin()
		}
		cancelDrain()
		p.cancelWorkers()
	})
	return p.closeResult
}

// closeWithContext stops admission, gives already admitted jobs a bounded
// drain window, then cancels them and waits through the bounded join window.
// The caller owns the enclosing hard cleanup deadline.
func (p *asyncPool) closeWithContext(ctx context.Context) (int, error) {
	if p == nil {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.beginClose()
	drainCtx, cancelDrain := context.WithTimeout(ctx, p.drainGrace)
	drained := p.waitWorkers(drainCtx)
	drainErr := drainCtx.Err()
	cancelDrain()
	if drained {
		p.cancelWorkers()
		return 0, nil
	}

	count := p.outstandingCount()
	p.cancelWorkers()
	if err := ctx.Err(); err != nil {
		return count, err
	}
	joinCtx, cancelJoin := context.WithTimeout(ctx, p.joinGrace)
	joined := p.waitWorkers(joinCtx)
	joinErr := joinCtx.Err()
	cancelJoin()
	if joined {
		return count, nil
	}
	if err := ctx.Err(); err != nil {
		return count, err
	}
	if joinErr != nil {
		return count, joinErr
	}
	return count, drainErr
}

func (p *asyncPool) beginClose() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		p.mu.Lock()
		p.accepting = false
		close(p.queue)
		p.mu.Unlock()
		go func() {
			p.workers.Wait()
			close(p.workersDone)
		}()
	})
}

func (p *asyncPool) waitWorkers(ctx context.Context) bool {
	if p == nil {
		return true
	}
	select {
	case <-p.workersDone:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *asyncPool) cancelWorkers() {
	if p != nil && p.cancel != nil {
		p.cancel()
	}
}

func (p *asyncPool) outstandingCount() int {
	if p == nil {
		return 0
	}
	count := int(p.outstanding.Load())
	if count < 0 {
		return 0
	}
	return count
}

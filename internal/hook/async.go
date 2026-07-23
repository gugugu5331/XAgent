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
	p := &asyncPool{accepting: true, queue: make(chan asyncJob, queueSize), root: root, cancel: cancel, drainGrace: drainGrace, joinGrace: joinGrace}
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
		p.mu.Lock()
		p.accepting = false
		close(p.queue)
		p.mu.Unlock()
		done := make(chan struct{})
		go func() { p.workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(p.drainGrace):
			p.closeResult = int(p.outstanding.Load())
			if p.closeResult < 0 {
				p.closeResult = 0
			}
			p.cancel()
			select {
			case <-done:
			case <-time.After(p.joinGrace):
			}
		}
		p.cancel()
	})
	return p.closeResult
}

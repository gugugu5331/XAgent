package stdio

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

const (
	writerQueueCapacity    = 1
	writerInFlightCapacity = 2
)

var (
	ErrWriteFailed       = errors.New("MCP stdio frame write failed")
	ErrWriterUnavailable = errors.New("MCP stdio writer is unavailable")
)

const (
	writerRequestQueued uint32 = iota
	writerRequestWriting
	writerRequestCancelled
	writerRequestDone
)

type writerRequest struct {
	frame  []byte
	result chan error
	state  atomic.Uint32
}

type writerWorker struct {
	writer  io.Writer
	onFatal func()
	queue   chan *writerRequest
	slots   chan struct{}
	stop    chan struct{}
	done    chan struct{}

	admissionMu sync.Mutex
	stopped     bool
	terminalErr error
	abortOnce   sync.Once
}

func startWriterWorker(writer io.Writer, onFatal func()) (*writerWorker, error) {
	if isNilInterface(writer) || onFatal == nil {
		return nil, ErrInvalidConfig
	}
	worker := &writerWorker{
		writer:  writer,
		onFatal: onFatal,
		queue:   make(chan *writerRequest, writerQueueCapacity),
		slots:   make(chan struct{}, writerInFlightCapacity),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	for index := 0; index < writerInFlightCapacity; index++ {
		worker.slots <- struct{}{}
	}
	go worker.run()
	return worker, nil
}

func (worker *writerWorker) Send(ctx context.Context, frame []byte) error {
	if worker == nil {
		return ErrWriterUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-worker.stop:
		return worker.failure()
	case <-worker.slots:
	}
	encoded := make([]byte, len(frame)+1)
	copy(encoded, frame)
	encoded[len(frame)] = '\n'
	request := &writerRequest{frame: encoded, result: make(chan error, 1)}

	worker.admissionMu.Lock()
	if worker.stopped {
		failure := worker.terminalErr
		worker.admissionMu.Unlock()
		worker.releaseSlot()
		clear(encoded)
		return failure
	}
	if err := ctx.Err(); err != nil {
		worker.admissionMu.Unlock()
		worker.releaseSlot()
		clear(encoded)
		return err
	}
	worker.queue <- request
	worker.admissionMu.Unlock()

	select {
	case result := <-request.result:
		return result
	default:
	}
	select {
	case result := <-request.result:
		return result
	case <-ctx.Done():
		return worker.cancelRequest(ctx, request)
	case <-worker.stop:
		select {
		case result := <-request.result:
			return result
		default:
		}
		switch request.state.Load() {
		case writerRequestWriting:
			select {
			case result := <-request.result:
				return result
			case <-ctx.Done():
				return worker.cancelRequest(ctx, request)
			}
		case writerRequestDone:
			return <-request.result
		}
		if ctx.Err() != nil {
			return worker.cancelRequest(ctx, request)
		}
		return worker.failure()
	}
}

func (worker *writerWorker) cancelRequest(ctx context.Context, request *writerRequest) error {
	if request.state.CompareAndSwap(writerRequestQueued, writerRequestCancelled) {
		return ctx.Err()
	}
	switch request.state.Load() {
	case writerRequestWriting:
		worker.abort()
		return ctx.Err()
	case writerRequestDone:
		return <-request.result
	default:
		return ctx.Err()
	}
}

func (worker *writerWorker) abort() {
	if worker == nil {
		return
	}
	worker.abortOnce.Do(worker.onFatal)
}

func (worker *writerWorker) Stop() {
	worker.stopWithError(ErrWriterUnavailable)
}

func (worker *writerWorker) stopWithError(failure error) {
	if worker == nil {
		return
	}
	worker.admissionMu.Lock()
	if !worker.stopped {
		worker.stopped = true
		worker.terminalErr = failure
		close(worker.stop)
	}
	worker.admissionMu.Unlock()
}

func (worker *writerWorker) failure() error {
	if worker == nil {
		return ErrWriterUnavailable
	}
	worker.admissionMu.Lock()
	defer worker.admissionMu.Unlock()
	if worker.terminalErr == nil {
		return ErrWriterUnavailable
	}
	return worker.terminalErr
}

func (worker *writerWorker) Done() <-chan struct{} {
	if worker == nil {
		return nil
	}
	return worker.done
}

func (worker *writerWorker) run() {
	defer close(worker.done)
	defer worker.rejectQueued()
	for {
		select {
		case <-worker.stop:
			return
		default:
		}
		select {
		case <-worker.stop:
			return
		case request := <-worker.queue:
			worker.admissionMu.Lock()
			if worker.stopped {
				failure := worker.terminalErr
				worker.admissionMu.Unlock()
				if request.state.CompareAndSwap(writerRequestQueued, writerRequestDone) {
					request.result <- failure
				}
				clear(request.frame)
				worker.releaseSlot()
				return
			}
			if !request.state.CompareAndSwap(writerRequestQueued, writerRequestWriting) {
				worker.admissionMu.Unlock()
				clear(request.frame)
				worker.releaseSlot()
				continue
			}
			worker.admissionMu.Unlock()
			result := writeStdioFrame(worker.writer, request.frame)
			clear(request.frame)
			request.state.Store(writerRequestDone)
			request.result <- result
			if result != nil {
				worker.stopWithError(ErrWriteFailed)
				worker.releaseSlot()
				worker.abort()
				return
			}
			worker.releaseSlot()
		}
	}
}

func (worker *writerWorker) rejectQueued() {
	for {
		select {
		case request := <-worker.queue:
			worker.releaseSlot()
			if request.state.CompareAndSwap(writerRequestQueued, writerRequestDone) {
				request.result <- worker.failure()
			}
			clear(request.frame)
		default:
			return
		}
	}
}

func (worker *writerWorker) releaseSlot() {
	worker.slots <- struct{}{}
}

func writeStdioFrame(writer io.Writer, frame []byte) error {
	for written := 0; written < len(frame); {
		count, err := writer.Write(frame[written:])
		if count < 0 || count > len(frame)-written {
			return ErrWriteFailed
		}
		written += count
		if err != nil || count == 0 {
			return ErrWriteFailed
		}
	}
	return nil
}

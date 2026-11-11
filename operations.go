package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"
)

var errOutcomeUnknown = errors.New("outcome unknown")

type OperationStep struct {
	Name     string     `json:"name"`
	Unit     string     `json:"unit,omitempty"`
	State    string     `json:"state"`
	Detail   string     `json:"detail,omitempty"`
	Error    string     `json:"error,omitempty"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
}

type Operation struct {
	ID       string          `json:"id"`
	Label    string          `json:"label"`
	Stack    string          `json:"stack,omitempty"`
	State    string          `json:"state"`
	Revision uint64          `json:"revision"`
	Created  time.Time       `json:"created"`
	Deadline time.Time       `json:"deadline"`
	Started  *time.Time      `json:"started,omitempty"`
	Finished *time.Time      `json:"finished,omitempty"`
	Error    string          `json:"error,omitempty"`
	Steps    []OperationStep `json:"steps"`
}

type operationTask struct {
	name    string
	unit    string
	timeout time.Duration
	run     func(context.Context, func(string)) error
}

type operationRun struct {
	Operation
	tasks           []operationTask
	continueOnError bool
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	err             error
	stopQueued      func() bool
}

type operationCheckKey struct{}

type Operations struct {
	mu      sync.Mutex
	history []*operationRun
	queue   []*operationRun
	running bool
}

func copyOperation(run *operationRun) Operation {
	operation := run.Operation
	operation.Steps = slices.Clone(operation.Steps)
	return operation
}

func (ops *Operations) Submit(ctx context.Context, label, stack string, tasks []operationTask, continueOnError bool) (Operation, error) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	active := 0
	for _, run := range ops.history {
		if run.Finished == nil {
			active++
		}
	}
	if active >= 16 {
		return Operation{}, errors.New("the action queue is full; wait for an existing action to finish")
	}
	timeout := 5 * time.Minute
	for _, task := range tasks {
		timeout = max(timeout, task.timeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	deadline, _ := ctx.Deadline()
	run := &operationRun{
		Operation: Operation{ID: randomHex(12), Label: label, Stack: stack, State: "queued", Revision: 1, Created: time.Now(), Deadline: deadline, Steps: make([]OperationStep, len(tasks))},
		tasks:     tasks, continueOnError: continueOnError, ctx: ctx, cancel: cancel, done: make(chan struct{}),
	}
	for index, task := range tasks {
		run.Steps[index] = OperationStep{Name: task.name, Unit: task.unit, State: "queued"}
	}
	for len(ops.history) >= 50 {
		index := slices.IndexFunc(ops.history, func(run *operationRun) bool { return run.Finished != nil })
		if index < 0 {
			break
		}
		ops.history = slices.Delete(ops.history, index, index+1)
	}
	ops.history = append(ops.history, run)
	ops.queue = append(ops.queue, run)
	run.stopQueued = context.AfterFunc(ctx, func() {
		ops.mu.Lock()
		defer ops.mu.Unlock()
		if run.State != "queued" {
			return
		}
		run.err = fmt.Errorf("action expired while queued; no command was sent: %w", ctx.Err())
		run.State, run.Error = operationState(run.err), run.err.Error()
		run.Revision++
		finished := time.Now()
		run.Finished = &finished
		for index := range run.Steps {
			run.Steps[index].State = "skipped"
		}
		ops.queue = slices.DeleteFunc(ops.queue, func(queued *operationRun) bool { return queued == run })
		run.tasks, run.stopQueued = nil, nil
		run.cancel()
		close(run.done)
	})
	if !ops.running {
		ops.running = true
		go ops.work()
	}
	return copyOperation(run), nil
}

func operationState(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed_out"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, errOutcomeUnknown) {
		return "unknown"
	}
	if err != nil {
		return "failed"
	}
	return "succeeded"
}

func (ops *Operations) work() {
	for {
		ops.mu.Lock()
		if len(ops.queue) == 0 {
			ops.running = false
			ops.mu.Unlock()
			return
		}
		run := ops.queue[0]
		ops.queue = ops.queue[1:]
		started := time.Now()
		run.State, run.Started = "running", &started
		run.Revision++
		run.stopQueued()
		run.stopQueued = nil
		ops.mu.Unlock()
		var failures []error
		for index, task := range run.tasks {
			err := run.ctx.Err()
			if err == nil {
				if check, ok := run.ctx.Value(operationCheckKey{}).(func() error); ok {
					err = check()
				}
			}
			if err == nil {
				ops.mu.Lock()
				started := time.Now()
				run.Steps[index].State, run.Steps[index].Started = "running", &started
				run.Revision++
				ops.mu.Unlock()
				err = task.run(run.ctx, func(detail string) {
					ops.mu.Lock()
					defer ops.mu.Unlock()
					run.Steps[index].Detail = tail([]byte(detail), 16<<10)
					run.Revision++
				})
			}
			ops.mu.Lock()
			finished := time.Now()
			run.Steps[index].State = operationState(err)
			run.Revision++
			run.Steps[index].Finished = &finished
			if err != nil {
				run.Steps[index].Error = err.Error()
				failures = append(failures, fmt.Errorf("%s: %w", task.name, err))
			}
			ops.mu.Unlock()
			if err != nil && (!run.continueOnError || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errOutcomeUnknown)) {
				break
			}
		}
		ops.mu.Lock()
		run.err = errors.Join(failures...)
		run.State = operationState(run.err)
		run.Revision++
		finished := time.Now()
		run.Finished = &finished
		if run.err != nil {
			run.Error = run.err.Error()
		}
		for index := range run.Steps {
			if run.Steps[index].State == "queued" {
				run.Steps[index].State = "skipped"
			}
		}
		run.tasks = nil
		run.cancel()
		close(run.done)
		ops.mu.Unlock()
	}
}

func (ops *Operations) List() []Operation {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	result := make([]Operation, 0, len(ops.history))
	for index := len(ops.history) - 1; index >= 0; index-- {
		result = append(result, copyOperation(ops.history[index]))
	}
	return result
}

func (ops *Operations) Get(id string) (Operation, bool) {
	ops.mu.Lock()
	defer ops.mu.Unlock()
	for _, run := range ops.history {
		if run.ID == id {
			return copyOperation(run), true
		}
	}
	return Operation{}, false
}

func (ops *Operations) Wait(ctx context.Context, id string) error {
	ops.mu.Lock()
	var found *operationRun
	for _, run := range ops.history {
		if run.ID == id {
			found = run
			break
		}
	}
	ops.mu.Unlock()
	if found == nil {
		return errors.New("action is no longer available")
	}
	select {
	case <-found.done:
		return found.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) perform(ctx context.Context, label, stack string, tasks []operationTask, continueOnError bool) error {
	operation, err := s.operations.Submit(ctx, label, stack, tasks, continueOnError)
	if err != nil {
		return err
	}
	return s.operations.Wait(ctx, operation.ID)
}

func simpleTask(name, unit string, run func(context.Context) error) operationTask {
	return operationTask{name: name, unit: unit, run: func(ctx context.Context, _ func(string)) error { return run(ctx) }}
}

func (s *Server) enqueue(w http.ResponseWriter, label, stack string, tasks []operationTask, continueOnError bool) {
	operation, err := s.operations.Submit(context.Background(), label, stack, tasks, continueOnError)
	if err != nil {
		writeErr(w, http.StatusTooManyRequests, err)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]any{"operation": operation})
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, s.operations.List())
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	operation, ok := s.operations.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("action is no longer available; check unit state before retrying"))
		return
	}
	writeJSON(w, operation)
}

type boundedBuffer struct {
	limit int
	data  []byte
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	size := len(data)
	if size >= b.limit {
		b.data = append(b.data[:0], data[size-b.limit:]...)
	} else {
		b.data = append(b.data, data...)
		if len(b.data) > b.limit {
			b.data = b.data[len(b.data)-b.limit:]
		}
	}
	return size, nil
}

type progressWriter struct {
	buffer boundedBuffer
	report func(string)
}

func (pw *progressWriter) Write(data []byte) (int, error) {
	size, err := pw.buffer.Write(data)
	pw.report(string(pw.buffer.data))
	return size, err
}

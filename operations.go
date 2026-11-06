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

func (operations *Operations) Submit(ctx context.Context, label, stack string, tasks []operationTask, continueOnError bool) (Operation, error) {
	operations.mu.Lock()
	defer operations.mu.Unlock()
	active := 0
	for _, run := range operations.history {
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
	for len(operations.history) >= 50 {
		index := slices.IndexFunc(operations.history, func(run *operationRun) bool { return run.Finished != nil })
		if index < 0 {
			break
		}
		operations.history = slices.Delete(operations.history, index, index+1)
	}
	operations.history = append(operations.history, run)
	operations.queue = append(operations.queue, run)
	run.stopQueued = context.AfterFunc(ctx, func() {
		operations.mu.Lock()
		defer operations.mu.Unlock()
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
		operations.queue = slices.DeleteFunc(operations.queue, func(queued *operationRun) bool { return queued == run })
		run.tasks, run.stopQueued = nil, nil
		run.cancel()
		close(run.done)
	})
	if !operations.running {
		operations.running = true
		go operations.work()
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

func (operations *Operations) work() {
	for {
		operations.mu.Lock()
		if len(operations.queue) == 0 {
			operations.running = false
			operations.mu.Unlock()
			return
		}
		run := operations.queue[0]
		operations.queue = operations.queue[1:]
		started := time.Now()
		run.State, run.Started = "running", &started
		run.Revision++
		run.stopQueued()
		run.stopQueued = nil
		operations.mu.Unlock()
		var failures []error
		for index, task := range run.tasks {
			err := run.ctx.Err()
			if err == nil {
				if check, ok := run.ctx.Value(operationCheckKey{}).(func() error); ok {
					err = check()
				}
			}
			if err == nil {
				operations.mu.Lock()
				started := time.Now()
				run.Steps[index].State, run.Steps[index].Started = "running", &started
				run.Revision++
				operations.mu.Unlock()
				err = task.run(run.ctx, func(detail string) {
					operations.mu.Lock()
					defer operations.mu.Unlock()
					run.Steps[index].Detail = tail([]byte(detail), 16<<10)
					run.Revision++
				})
			}
			operations.mu.Lock()
			finished := time.Now()
			run.Steps[index].State = operationState(err)
			run.Revision++
			run.Steps[index].Finished = &finished
			if err != nil {
				run.Steps[index].Error = err.Error()
				failures = append(failures, fmt.Errorf("%s: %w", task.name, err))
			}
			operations.mu.Unlock()
			if err != nil && (!run.continueOnError || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errOutcomeUnknown)) {
				break
			}
		}
		operations.mu.Lock()
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
		operations.mu.Unlock()
	}
}

func (operations *Operations) List() []Operation {
	operations.mu.Lock()
	defer operations.mu.Unlock()
	result := make([]Operation, 0, len(operations.history))
	for index := len(operations.history) - 1; index >= 0; index-- {
		result = append(result, copyOperation(operations.history[index]))
	}
	return result
}

func (operations *Operations) Get(id string) (Operation, bool) {
	operations.mu.Lock()
	defer operations.mu.Unlock()
	for _, run := range operations.history {
		if run.ID == id {
			return copyOperation(run), true
		}
	}
	return Operation{}, false
}

func (operations *Operations) Wait(ctx context.Context, id string) error {
	operations.mu.Lock()
	var found *operationRun
	for _, run := range operations.history {
		if run.ID == id {
			found = run
			break
		}
	}
	operations.mu.Unlock()
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

func (server *Server) perform(ctx context.Context, label, stack string, tasks []operationTask, continueOnError bool) error {
	operation, err := server.operations.Submit(ctx, label, stack, tasks, continueOnError)
	if err != nil {
		return err
	}
	return server.operations.Wait(ctx, operation.ID)
}

func simpleTask(name, unit string, run func(context.Context) error) operationTask {
	return operationTask{name: name, unit: unit, run: func(ctx context.Context, _ func(string)) error { return run(ctx) }}
}

func (server *Server) enqueue(writer http.ResponseWriter, label, stack string, tasks []operationTask, continueOnError bool) {
	operation, err := server.operations.Submit(context.Background(), label, stack, tasks, continueOnError)
	if err != nil {
		writeErr(writer, http.StatusTooManyRequests, err)
		return
	}
	writeJSONStatus(writer, http.StatusAccepted, map[string]any{"operation": operation})
}

func (server *Server) listOperations(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, server.operations.List())
}

func (server *Server) getOperation(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	operation, ok := server.operations.Get(request.PathValue("id"))
	if !ok {
		writeErr(writer, http.StatusNotFound, errors.New("action is no longer available; check unit state before retrying"))
		return
	}
	writeJSON(writer, operation)
}

type boundedBuffer struct {
	limit int
	data  []byte
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	size := len(data)
	if size >= buffer.limit {
		buffer.data = append(buffer.data[:0], data[size-buffer.limit:]...)
	} else {
		buffer.data = append(buffer.data, data...)
		if len(buffer.data) > buffer.limit {
			buffer.data = buffer.data[len(buffer.data)-buffer.limit:]
		}
	}
	return size, nil
}

type progressWriter struct {
	buffer boundedBuffer
	report func(string)
}

func (writer *progressWriter) Write(data []byte) (int, error) {
	size, err := writer.buffer.Write(data)
	writer.report(string(writer.buffer.data))
	return size, err
}

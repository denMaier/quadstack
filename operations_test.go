package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func waitOperation(t *testing.T, operations *Operations, id string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return operations.Wait(ctx, id)
}

func TestQueuedOperationExpiresWithoutStarting(t *testing.T) {
	var operations Operations
	release := make(chan struct{})
	first, err := operations.Submit(context.Background(), "blocked", "", []operationTask{simpleTask("block", "", func(context.Context) error { <-release; return nil })}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(release); waitOperation(t, &operations, first.ID) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var called atomic.Bool
	queued, err := operations.Submit(ctx, "queued", "", []operationTask{simpleTask("must not run", "", func(context.Context) error { called.Store(true); return nil })}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitOperation(t, &operations, queued.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queue expiration: %v", err)
	}
	final, _ := operations.Get(queued.ID)
	if final.Started != nil || final.State != "timed_out" || final.Steps[0].State != "skipped" || called.Load() {
		t.Fatalf("expired operation ran: %+v", final)
	}
}

func TestQueuedOperationRechecksAuthorization(t *testing.T) {
	var operations Operations
	release := make(chan struct{})
	first, err := operations.Submit(context.Background(), "blocked", "", []operationTask{simpleTask("block", "", func(context.Context) error { <-release; return nil })}, false)
	if err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Bool
	allowed.Store(true)
	ctx := context.WithValue(context.Background(), operationCheckKey{}, func() error {
		if !allowed.Load() {
			return errDenied{"grant ended"}
		}
		return nil
	})
	var called atomic.Bool
	queued, err := operations.Submit(ctx, "authorized at submit", "", []operationTask{simpleTask("must not run", "", func(context.Context) error { called.Store(true); return nil })}, false)
	if err != nil {
		t.Fatal(err)
	}
	allowed.Store(false)
	close(release)
	if err := waitOperation(t, &operations, first.ID); err != nil {
		t.Fatal(err)
	}
	err = waitOperation(t, &operations, queued.ID)
	if !errors.As(err, new(errDenied)) || called.Load() {
		t.Fatalf("expired grant allowed execution: %v", err)
	}
}

func TestOperationsQueueAndSnapshots(t *testing.T) {
	var operations Operations
	started, release := make(chan struct{}), make(chan struct{})
	first, err := operations.Submit(context.Background(), "first", "d/web", []operationTask{simpleTask("start", "web.service", func(context.Context) error { close(started); <-release; return nil })}, false)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := operations.Submit(context.Background(), "second", "", []operationTask{simpleTask("reload", "", func(context.Context) error { return nil })}, false)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != "queued" {
		t.Fatalf("second: %+v", second)
	}
	copy, _ := operations.Get(first.ID)
	copy.Steps[0].State = "changed"
	original, _ := operations.Get(first.ID)
	if original.Steps[0].State != "running" {
		t.Fatal("snapshot mutated live state")
	}
	close(release)
	if err := waitOperation(t, &operations, second.ID); err != nil {
		t.Fatal(err)
	}
	for _, operation := range operations.List() {
		if operation.State != "succeeded" || operation.Steps[0].State != "succeeded" || operation.Finished == nil || operation.Revision <= first.Revision {
			t.Fatalf("incomplete operation: %+v", operation)
		}
	}
}

func TestOperationsFailureAndTimeout(t *testing.T) {
	for _, keepGoing := range []bool{false, true} {
		var operations Operations
		called := false
		operation, err := operations.Submit(context.Background(), "failure", "", []operationTask{
			simpleTask("fail", "web.service", func(context.Context) error { return errors.New("failure") }),
			simpleTask("next", "", func(context.Context) error { called = true; return nil }),
		}, keepGoing)
		if err != nil {
			t.Fatal(err)
		}
		if err := waitOperation(t, &operations, operation.ID); err == nil {
			t.Fatal("failure was swallowed")
		}
		final, _ := operations.Get(operation.ID)
		if called != keepGoing || final.State != "failed" {
			t.Fatalf("continue=%t: %+v", keepGoing, final)
		}
		if !keepGoing && final.Steps[1].State != "skipped" {
			t.Fatal("unsafe dependent step ran")
		}
	}
	var operations Operations
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	operation, err := operations.Submit(ctx, "timeout", "", []operationTask{simpleTask("wait", "", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitOperation(t, &operations, operation.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error: %v", err)
	}
	final, _ := operations.Get(operation.ID)
	if final.State != "timed_out" {
		t.Fatalf("timeout: %+v", final)
	}
}

func TestOperationsBoundedQueueAndHistory(t *testing.T) {
	var operations Operations
	release := make(chan struct{})
	var last Operation
	for index := 0; index < 16; index++ {
		operation, err := operations.Submit(context.Background(), "queued", "", []operationTask{simpleTask("wait", "", func(context.Context) error { <-release; return nil })}, false)
		if err != nil {
			t.Fatal(err)
		}
		last = operation
	}
	if _, err := operations.Submit(context.Background(), "overflow", "", nil, false); err == nil {
		t.Fatal("queue is unbounded")
	}
	close(release)
	if err := waitOperation(t, &operations, last.ID); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 55; index++ {
		operation, err := operations.Submit(context.Background(), "completed", "", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := waitOperation(t, &operations, operation.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(operations.List()) != 50 {
		t.Fatalf("history length: %d", len(operations.List()))
	}
}

package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
)

type fakeSystemdConnection struct {
	connected atomic.Bool
	closed    atomic.Bool
	start     func(context.Context, string, chan<- string) error
}

func newFakeSystemdConnection() *fakeSystemdConnection {
	conn := &fakeSystemdConnection{}
	conn.connected.Store(true)
	return conn
}

func (conn *fakeSystemdConnection) Connected() bool { return conn.connected.Load() }
func (conn *fakeSystemdConnection) Close()          { conn.closed.Store(true); conn.connected.Store(false) }
func (conn *fakeSystemdConnection) ListUnitsByNamesContext(context.Context, []string) ([]dbus.UnitStatus, error) {
	return nil, nil
}
func (conn *fakeSystemdConnection) ReloadContext(context.Context) error { return nil }
func (conn *fakeSystemdConnection) GetUnitTypePropertiesContext(context.Context, string, string) (map[string]any, error) {
	return map[string]any{"Result": "exit-code", "ExecMainStatus": int32(42)}, nil
}
func (conn *fakeSystemdConnection) StartUnitContext(ctx context.Context, unit, mode string, done chan<- string) (int, error) {
	if conn.start != nil {
		return 1, conn.start(ctx, unit, done)
	}
	done <- "done"
	return 1, nil
}
func (conn *fakeSystemdConnection) StopUnitContext(ctx context.Context, unit, mode string, done chan<- string) (int, error) {
	return conn.StartUnitContext(ctx, unit, mode, done)
}
func (conn *fakeSystemdConnection) RestartUnitContext(ctx context.Context, unit, mode string, done chan<- string) (int, error) {
	return conn.StartUnitContext(ctx, unit, mode, done)
}

func TestSystemdConnectionOutlivesRequest(testingT *testing.T) {
	conn := newFakeSystemdConnection()
	var lifetime context.Context
	dials := 0
	systemd := &Systemd{dial: func(ctx context.Context) (systemdConnection, error) { lifetime = ctx; dials++; return conn, nil }}
	testingT.Cleanup(systemd.Close)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := systemd.Status(ctx, []string{"web.service"}); err != nil {
		testingT.Fatal(err)
	}
	cancel()
	if lifetime.Err() != nil {
		testingT.Fatalf("request cancellation closed the shared connection: %v", lifetime.Err())
	}
	if err := systemd.Action(context.Background(), "web.service", "start"); err != nil {
		testingT.Fatal(err)
	}
	if dials != 1 {
		testingT.Fatalf("connection was not reused: %d dials", dials)
	}
	systemd.Close()
	if lifetime.Err() == nil || !conn.closed.Load() {
		testingT.Fatal("Close did not release the connection")
	}
}

func TestSystemdDialCancellation(testingT *testing.T) {
	systemd := &Systemd{dial: func(ctx context.Context) (systemdConnection, error) { <-ctx.Done(); return nil, ctx.Err() }}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := systemd.get(ctx); !errors.Is(err, context.DeadlineExceeded) {
		testingT.Fatalf("dial error: %v", err)
	}
}

func TestSystemdReconnect(testingT *testing.T) {
	old, fresh := newFakeSystemdConnection(), newFakeSystemdConnection()
	old.connected.Store(false)
	previous, cancel := context.WithCancel(context.Background())
	systemd := &Systemd{conn: old, cancel: cancel, dial: func(context.Context) (systemdConnection, error) { return fresh, nil }}
	testingT.Cleanup(systemd.Close)
	if err := systemd.Action(context.Background(), "web.service", "start"); err != nil {
		testingT.Fatal(err)
	}
	if !old.closed.Load() || previous.Err() == nil {
		testingT.Fatal("stale connection was not released")
	}
}

func TestSystemdDisconnectWhileWaiting(testingT *testing.T) {
	conn := newFakeSystemdConnection()
	conn.start = func(context.Context, string, chan<- string) error { conn.connected.Store(false); return nil }
	systemd := &Systemd{conn: conn}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := systemd.Action(ctx, "web.service", "start")
	if err == nil || !strings.Contains(err.Error(), "connection to systemd was lost") {
		testingT.Fatalf("disconnect error: %v", err)
	}
}

func TestSystemdJobResults(testingT *testing.T) {
	for _, result := range []string{"done", "failed", "dependency", "canceled", "timeout"} {
		testingT.Run(result, func(testingT *testing.T) {
			conn := newFakeSystemdConnection()
			conn.start = func(_ context.Context, _ string, done chan<- string) error { done <- result; return nil }
			err := (&Systemd{conn: conn}).Action(context.Background(), "web.service", "restart")
			if result == "done" {
				if err != nil {
					testingT.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "exit status: 42") {
				testingT.Fatalf("missing service diagnostics: %v", err)
			}
			if result == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				testingT.Fatalf("timeout not preserved: %v", err)
			}
		})
	}
}

func TestSystemdWaitTimeoutDoesNotRetry(testingT *testing.T) {
	conn := newFakeSystemdConnection()
	calls := 0
	conn.start = func(context.Context, string, chan<- string) error { calls++; return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := (&Systemd{conn: conn}).Action(ctx, "web.service", "restart")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "may still be running") || calls != 1 {
		testingT.Fatalf("timeout error: %v, calls: %d", err, calls)
	}
}

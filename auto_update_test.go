package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
)

func TestStackAutoUpdate(t *testing.T) {
	stack := &Stack{Files: []*QFile{{Kind: "container", Unit: "app.service"}, {Kind: "container", Unit: "db.service"}, {Kind: "pod", Unit: "app-pod.service"}}}
	container := func(unit, policy string) Container {
		return Container{Labels: map[string]string{"PODMAN_SYSTEMD_UNIT": unit, "io.containers.autoupdate": policy}}
	}
	for _, test := range []struct {
		name       string
		containers []Container
		available  bool
		want       string
	}{
		{"registry and local", []Container{container("app.service", "registry"), container("db.service", "local")}, true, "on"},
		{"legacy policy", []Container{container("app.service", "image"), container("db.service", "registry")}, true, "on"},
		{"off", []Container{container("app.service", ""), container("db.service", "disabled")}, true, "off"},
		{"mixed", []Container{container("app.service", "registry"), container("db.service", "")}, true, "mixed"},
		{"missing container", []Container{container("app.service", "registry")}, true, "unknown"},
		{"never deployed", nil, true, "unknown"},
		{"unavailable Podman", nil, false, "unknown"},
		{"invalid policy", []Container{container("app.service", "invalid"), container("db.service", "registry")}, true, "unknown"},
		{"unrelated container", []Container{container("app.service", ""), container("db.service", ""), container("other.service", "registry")}, true, "off"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := stackAutoUpdate(stack, test.containers, test.available); got != test.want {
				t.Fatalf("got %s, want %s", got, test.want)
			}
		})
	}
	if got := stackAutoUpdate(&Stack{Files: []*QFile{{Kind: "volume"}}}, nil, false); got != "none" {
		t.Fatalf("resource-only stack: %s", got)
	}
	kube := &Stack{Files: []*QFile{{Kind: "kube", Unit: "app.service"}}}
	if got := stackAutoUpdate(kube, []Container{container("app.service", "registry"), container("app.service", "")}, true); got != "mixed" {
		t.Fatalf("multiple containers from one kube unit: %s", got)
	}
}

func TestAutoUpdateAPI(t *testing.T) {
	server, cookie := testServer(t)
	handler := server.Routes()
	if w := do(t, handler, nil, "GET", "/api/auto-update", "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("status without session: %d", w.Code)
	}
	if w := do(t, handler, nil, "PUT", "/api/auto-update", `{"enabled":true}`, true); w.Code != http.StatusUnauthorized {
		t.Fatalf("change without session: %d", w.Code)
	}
	if w := do(t, handler, cookie, "PUT", "/api/auto-update", `{"enabled":true}`, false); w.Code != http.StatusForbidden {
		t.Fatalf("change without CSRF header: %d", w.Code)
	}
	for _, body := range []string{`{}`, `{"enabled":null}`, `{"enabled":"yes"}`, `invalid`} {
		if w := do(t, handler, cookie, "PUT", "/api/auto-update", body, true); w.Code != http.StatusBadRequest {
			t.Errorf("invalid request %s: %d", body, w.Code)
		}
	}
	server.systemd = &fakeSystemdClient{autoUpdate: func(context.Context) (AutoUpdateTimer, error) {
		return AutoUpdateTimer{}, errors.New("systemd unavailable")
	}}
	if w := do(t, handler, cookie, "GET", "/api/auto-update", "", false); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "systemd unavailable") {
		t.Fatalf("unavailable systemd: %d %s", w.Code, w.Body)
	}
}

func TestAutoUpdateOperations(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, failure := range []string{"", "enablement", "reload", "start", "stop", "verify"} {
			if enabled && failure == "stop" || !enabled && failure == "start" {
				continue
			}
			t.Run(fmt.Sprintf("enabled=%t/fail=%s", enabled, failure), func(t *testing.T) {
				server, cookie := testServer(t)
				var calls []string
				call := func(name string) error {
					calls = append(calls, name)
					if name == failure {
						return errors.New(name + " failed")
					}
					return nil
				}
				server.systemd = &fakeSystemdClient{
					enablement: func(_ context.Context, value bool) error {
						if value != enabled {
							t.Errorf("enablement = %t, want %t", value, enabled)
						}
						return call("enablement")
					},
					reload: func(context.Context) error { return call("reload") },
					action: func(_ context.Context, unit, action string) error {
						if unit != autoUpdateTimerUnit {
							t.Errorf("acted on %q", unit)
						}
						return call(action)
					},
					autoUpdate: func(context.Context) (AutoUpdateTimer, error) {
						calls = append(calls, "verify")
						state := AutoUpdateTimer{Load: "loaded", UnitFileState: "disabled", Active: "inactive"}
						if enabled != (failure == "verify") {
							state.UnitFileState, state.Active, state.Enabled = "enabled", "active", true
						}
						return state, nil
					},
				}
				operation := responseOperation(t, do(t, server.Routes(), cookie, "PUT", "/api/auto-update", fmt.Sprintf(`{"enabled":%t}`, enabled), true))
				err := waitOperation(t, &server.operations, operation.ID)
				if (err != nil) != (failure != "") {
					t.Fatalf("error: %v", err)
				}
				want := []string{"enablement", "reload", "start", "verify"}
				if !enabled {
					want = []string{"stop", "enablement", "reload", "verify"}
				}
				if failure != "" {
					for i, name := range want {
						if name == failure {
							want = want[:i+1]
							break
						}
					}
				}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("calls: %v, want %v", calls, want)
				}
			})
		}
	}
}

func TestSystemdAutoUpdateTimer(t *testing.T) {
	conn := newFakeSystemdConnection()
	conn.list = func(context.Context, []string) ([]dbus.UnitStatus, error) {
		return []dbus.UnitStatus{
			{Name: autoUpdateTimerUnit, LoadState: "loaded", ActiveState: "active"},
			{Name: autoUpdateServiceUnit, LoadState: "loaded", ActiveState: "inactive"},
		}, nil
	}
	next := time.Date(2026, 11, 14, 0, 5, 0, 0, time.UTC)
	last := next.Add(-24 * time.Hour)
	conn.properties = func(_ context.Context, name, kind string) (map[string]any, error) {
		switch kind {
		case "Unit":
			return map[string]any{"UnitFileState": "enabled"}, nil
		case "Timer":
			return map[string]any{
				"TimersCalendar":         [][]any{{"OnCalendar", "*-*-* 00:00:00", uint64(next.UnixMicro())}},
				"NextElapseUSecRealtime": uint64(next.UnixMicro()),
				"RandomizedDelayUSec":    uint64(900_000_000),
			}, nil
		case "Service":
			return map[string]any{"ExecMainStartTimestamp": uint64(last.UnixMicro()), "Result": "exit-code"}, nil
		}
		return nil, fmt.Errorf("unexpected property request %s/%s", name, kind)
	}
	state, err := (&Systemd{conn: conn}).AutoUpdateTimer(context.Background())
	if err != nil || !state.Enabled || state.RandomizedDelay != 900 || !reflect.DeepEqual(state.Schedule, []string{"*-*-* 00:00:00"}) || state.NextRun == nil || !state.NextRun.Equal(next) || state.LastRun == nil || !state.LastRun.Equal(last) || state.LastResult != "exit-code" {
		t.Fatalf("state: %+v, error: %v", state, err)
	}
	for _, value := range []any{uint64(0), uint64(math.MaxUint64), nil} {
		if got := systemdTime(value); got != nil {
			t.Errorf("invalid systemd time %v: %v", value, got)
		}
	}
	conn.list = func(context.Context, []string) ([]dbus.UnitStatus, error) {
		return []dbus.UnitStatus{{Name: autoUpdateTimerUnit, LoadState: "not-found", ActiveState: "inactive"}}, nil
	}
	conn.properties = func(context.Context, string, string) (map[string]any, error) {
		t.Fatal("read properties of a missing timer")
		return nil, nil
	}
	if state, err := (&Systemd{conn: conn}).AutoUpdateTimer(context.Background()); err != nil || state.Load != "not-found" {
		t.Fatalf("missing timer: %+v, %v", state, err)
	}
}

func TestSystemdAutoUpdateEnablement(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		for _, disconnect := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%t/disconnect=%t", enabled, disconnect), func(t *testing.T) {
				conn := newFakeSystemdConnection()
				var calls []bool
				check := func(files []string, runtime, force bool) error {
					if !reflect.DeepEqual(files, []string{autoUpdateTimerUnit}) || force {
						t.Fatalf("unexpected unit files/force: %v %t", files, force)
					}
					calls = append(calls, runtime)
					if disconnect {
						conn.connected.Store(false)
						return errors.New("connection lost")
					}
					return nil
				}
				conn.enable = func(_ context.Context, files []string, runtime, force bool) (bool, []dbus.EnableUnitFileChange, error) {
					return true, nil, check(files, runtime, force)
				}
				conn.disable = func(_ context.Context, files []string, runtime bool) ([]dbus.DisableUnitFileChange, error) {
					return nil, check(files, runtime, false)
				}
				err := (&Systemd{conn: conn}).AutoUpdateEnablement(context.Background(), enabled)
				if disconnect != errors.Is(err, errOutcomeUnknown) {
					t.Fatalf("disconnect outcome: %v", err)
				}
				want := []bool{false}
				if !enabled && !disconnect {
					want = append(want, true)
				}
				if !reflect.DeepEqual(calls, want) {
					t.Fatalf("runtime flags: %v, want %v", calls, want)
				}
			})
		}
	}
}

// Run only as a disposable Linux user's systemd instance with Podman installed.
// This test changes the real timer, which must initially be disabled and stopped.
func TestSystemdAutoUpdateIntegration(t *testing.T) {
	if os.Getenv("QS_TEST_SYSTEMD") != "1" {
		t.Skip("set QS_TEST_SYSTEMD=1 in a disposable systemd/Podman test account")
	}
	systemd := &Systemd{}
	t.Cleanup(systemd.Close)
	ctx := context.Background()
	initial, err := systemd.AutoUpdateTimer(ctx)
	if err != nil || initial.Load != "loaded" || initial.Enabled || initial.Active != "inactive" {
		t.Fatalf("requires an installed, disabled and stopped user timer: %+v, %v", initial, err)
	}
	t.Cleanup(func() {
		systemd.Action(ctx, autoUpdateTimerUnit, "stop")
		systemd.AutoUpdateEnablement(ctx, false)
		systemd.Reload(ctx)
	})
	server, cookie := testServer(t)
	server.systemd = systemd
	change := func(enabled bool) AutoUpdateTimer {
		t.Helper()
		operation := responseOperation(t, do(t, server.Routes(), cookie, "PUT", "/api/auto-update", fmt.Sprintf(`{"enabled":%t}`, enabled), true))
		if err := waitOperation(t, &server.operations, operation.ID); err != nil {
			t.Fatal(err)
		}
		state, err := systemd.AutoUpdateTimer(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("timer enabled=%t, active=%s, schedule=%v, next=%v", state.Enabled, state.Active, state.Schedule, state.NextRun)
		return state
	}
	state := change(true)
	if !state.Enabled || state.NextRun == nil || len(state.Schedule) == 0 {
		t.Fatalf("enabled timer has no next run or schedule: %+v", state)
	}
	if err := systemd.Action(ctx, autoUpdateServiceUnit, "start"); err != nil {
		t.Fatal(err)
	}
	state, err = systemd.AutoUpdateTimer(ctx)
	if err != nil || state.LastRun == nil || state.LastResult != "success" || state.Running {
		t.Fatalf("last update result: %+v, %v", state, err)
	}
	change(false)
	// Disabling must remove enablement created with `systemctl --user enable --runtime` too.
	conn, err := systemd.get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.EnableUnitFilesContext(ctx, []string{autoUpdateTimerUnit}, true, false); err != nil {
		t.Fatal(err)
	}
	if err := systemd.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if err := systemd.Action(ctx, autoUpdateTimerUnit, "start"); err != nil {
		t.Fatal(err)
	}
	state = change(false)
	if state.Enabled || state.Active != "inactive" || state.NextRun != nil {
		t.Fatalf("runtime timer was not disabled: %+v", state)
	}
}

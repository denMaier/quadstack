package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/godbus/dbus/v5"
)

const autoUpdateTimerUnit = "podman-auto-update.timer"
const autoUpdateServiceUnit = "podman-auto-update.service"

type AutoUpdateTimer struct {
	Load            string     `json:"load"`
	Active          string     `json:"active"`
	UnitFileState   string     `json:"unitFileState"`
	Enabled         bool       `json:"enabled"`
	Schedule        []string   `json:"schedule"`
	RandomizedDelay uint64     `json:"randomizedDelay"` // seconds
	NextRun         *time.Time `json:"nextRun,omitempty"`
	LastRun         *time.Time `json:"lastRun,omitempty"`
	LastResult      string     `json:"lastResult,omitempty"`
	Running         bool       `json:"running"`
}

func (s *Systemd) AutoUpdateTimer(ctx context.Context) (AutoUpdateTimer, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	state := AutoUpdateTimer{Schedule: []string{}}
	// Listing by name loads the units before their D-Bus properties are read.
	units, err := s.Status(ctx, []string{autoUpdateTimerUnit, autoUpdateServiceUnit})
	if err != nil {
		return state, err
	}
	state.Load, state.Active = units[autoUpdateTimerUnit].Load, units[autoUpdateTimerUnit].Active
	if state.Load != "loaded" {
		return state, nil
	}
	c, err := s.get(ctx)
	if err != nil {
		return state, err
	}
	props, err := c.GetUnitPropertiesContext(ctx, autoUpdateTimerUnit)
	if err != nil {
		return state, err
	}
	state.UnitFileState, _ = props["UnitFileState"].(string)
	state.Enabled = state.UnitFileState == "enabled" || state.UnitFileState == "enabled-runtime"
	props, err = c.GetUnitTypePropertiesContext(ctx, autoUpdateTimerUnit, "Timer")
	if err != nil {
		return state, err
	}
	var calendar []struct {
		Base string
		Spec string
		Next uint64
	}
	if value, ok := props["TimersCalendar"]; ok {
		if err := dbus.Store([]any{value}, &calendar); err != nil {
			return state, fmt.Errorf("reading the auto-update schedule: %w", err)
		}
	}
	for _, entry := range calendar {
		state.Schedule = append(state.Schedule, entry.Spec)
	}
	delay, _ := props["RandomizedDelayUSec"].(uint64)
	state.RandomizedDelay = delay / 1_000_000
	if state.Active == "active" {
		state.NextRun = systemdTime(props["NextElapseUSecRealtime"])
	}
	service := units[autoUpdateServiceUnit]
	state.Running = service.Active == "active" || service.Active == "activating" || service.Active == "deactivating"
	if service.Load == "loaded" {
		props, err = c.GetUnitTypePropertiesContext(ctx, autoUpdateServiceUnit, "Service")
		if err != nil {
			return state, err
		}
		state.LastRun = systemdTime(props["ExecMainStartTimestamp"])
		if state.LastRun != nil {
			state.LastResult, _ = props["Result"].(string)
		}
	}
	return state, nil
}

func systemdTime(value any) *time.Time {
	usec, ok := value.(uint64)
	if !ok || usec == 0 || usec > math.MaxInt64 {
		return nil
	}
	t := time.UnixMicro(int64(usec)).UTC()
	return &t
}

// AutoUpdateEnablement only changes the shipped timer's enablement. The caller records
// reload and start/stop separately, so partial failures remain visible in Activity.
func (s *Systemd) AutoUpdateEnablement(ctx context.Context, enabled bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := s.get(ctx)
	if err != nil {
		return err
	}
	change := func(runtime bool) error {
		var err error
		if enabled {
			_, _, err = c.EnableUnitFilesContext(ctx, []string{autoUpdateTimerUnit}, false, false)
		} else {
			_, err = c.DisableUnitFilesContext(ctx, []string{autoUpdateTimerUnit}, runtime)
		}
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("changing timer enablement: %w; check its state before retrying", ctx.Err())
			}
			if !c.Connected() {
				return fmt.Errorf("changing timer enablement: %w; %w", err, errOutcomeUnknown)
			}
		}
		return err
	}
	if err := change(false); err != nil {
		return err
	}
	if !enabled {
		return change(true)
	}
	return nil
}

func (s *Server) autoUpdate(w http.ResponseWriter, r *http.Request) {
	state, err := s.systemd.AutoUpdateTimer(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("reading the auto-update timer: %w", err))
		return
	}
	writeJSON(w, state)
}

func (s *Server) setAutoUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Enabled == nil {
		writeErr(w, http.StatusBadRequest, errors.New("enabled must be true or false"))
		return
	}
	enabled := *req.Enabled
	label, action := "Disable scheduled updates", "stop"
	if enabled {
		label, action = "Enable scheduled updates", "start"
	}
	enablement := simpleTask("Change timer enablement", autoUpdateTimerUnit, func(ctx context.Context) error {
		return s.systemd.AutoUpdateEnablement(ctx, enabled)
	})
	reload := simpleTask("daemon-reload", "", s.systemd.Reload)
	job := simpleTask(action+" "+autoUpdateTimerUnit, autoUpdateTimerUnit, func(ctx context.Context) error {
		return s.systemd.Action(ctx, autoUpdateTimerUnit, action)
	})
	tasks := []operationTask{enablement, reload, job}
	if !enabled {
		tasks = []operationTask{job, enablement, reload}
	}
	tasks = append(tasks, simpleTask("Verify timer state", autoUpdateTimerUnit, func(ctx context.Context) error {
		state, err := s.systemd.AutoUpdateTimer(ctx)
		if err != nil {
			return err
		}
		if enabled && state.UnitFileState == "enabled" && state.Active == "active" {
			return nil
		}
		if !enabled && !state.Enabled && state.Active == "inactive" {
			return nil
		}
		return fmt.Errorf("timer state does not match the request: enablement %q, active %q; inspect its state and Activity before retrying", state.UnitFileState, state.Active)
	}))
	s.enqueue(w, label, "", tasks, false)
}

// stackAutoUpdate reports deployed policy rather than guessing at effective Quadlet drop-ins.
// A unit with no container yet cannot be assumed to have auto updates turned off.
func stackAutoUpdate(stack *Stack, containers []Container, available bool) string {
	wanted := map[string]bool{}
	for _, f := range stack.Files {
		if f.Kind == "container" || f.Kind == "kube" {
			wanted[f.Unit] = false
		}
	}
	if len(wanted) == 0 {
		return "none"
	}
	if !available {
		return "unknown"
	}
	on, off := false, false
	for _, c := range containers {
		if _, ok := wanted[c.Unit()]; !ok {
			continue
		}
		wanted[c.Unit()] = true
		switch c.Labels["io.containers.autoupdate"] {
		case "registry", "local", "image":
			on = true
		case "", "disabled":
			off = true
		default:
			return "unknown"
		}
	}
	for _, found := range wanted {
		if !found {
			return "unknown"
		}
	}
	if on && off {
		return "mixed"
	}
	if on {
		return "on"
	}
	return "off"
}

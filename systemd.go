package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
)

type systemdConnection interface {
	Connected() bool
	Close()
	ListUnitsByNamesContext(context.Context, []string) ([]dbus.UnitStatus, error)
	StartUnitContext(context.Context, string, string, chan<- string) (int, error)
	StopUnitContext(context.Context, string, string, chan<- string) (int, error)
	RestartUnitContext(context.Context, string, string, chan<- string) (int, error)
	GetUnitTypePropertiesContext(context.Context, string, string) (map[string]any, error)
	GetUnitPropertiesContext(context.Context, string) (map[string]any, error)
	EnableUnitFilesContext(context.Context, []string, bool, bool) (bool, []dbus.EnableUnitFileChange, error)
	DisableUnitFilesContext(context.Context, []string, bool) ([]dbus.DisableUnitFileChange, error)
	ReloadContext(context.Context) error
}

type systemdClient interface {
	Status(context.Context, []string) (map[string]UnitState, error)
	Action(context.Context, string, string) error
	Reload(context.Context) error
	AutoUpdateTimer(context.Context) (AutoUpdateTimer, error)
	AutoUpdateEnablement(context.Context, bool) error
}

// Systemd talks to the user's systemd instance over D-Bus
type Systemd struct {
	mu     sync.Mutex
	conn   systemdConnection
	cancel context.CancelFunc
	dial   func(context.Context) (systemdConnection, error)
}

func (s *Systemd) get(ctx context.Context) (systemdConnection, error) {
	ctx, stopDial := context.WithTimeout(ctx, 10*time.Second)
	defer stopDial()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.conn != nil && s.conn.Connected() {
		return s.conn, nil
	}
	s.close()
	lifetime, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, cancel)
	dial := s.dial
	if dial == nil {
		dial = func(ctx context.Context) (systemdConnection, error) {
			conn, err := dbus.NewUserConnectionContext(ctx)
			if err != nil {
				return nil, err
			}
			return conn, nil
		}
	}
	c, err := dial(lifetime)
	stopped := stop()
	if !stopped || ctx.Err() != nil || err != nil {
		cancel()
		if c != nil {
			c.Close()
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("connecting to the systemd user instance: %w", err)
	}
	s.conn, s.cancel = c, cancel
	return c, nil
}

func (s *Systemd) close() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

func (s *Systemd) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.close()
}

func checkAction(action string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("action must be start, stop or restart")
	}
	return nil
}

type UnitState struct {
	Load   string `json:"load"`   // loaded, not-found, …
	Active string `json:"active"` // active, inactive, failed, activating, …
	Sub    string `json:"sub"`
}

func (s *Systemd) Status(ctx context.Context, units []string) (map[string]UnitState, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := map[string]UnitState{}
	if len(units) == 0 {
		return out, nil
	}
	c, err := s.get(ctx)
	if err != nil {
		return nil, err
	}
	list, err := c.ListUnitsByNamesContext(ctx, units)
	if err != nil {
		return nil, err
	}
	for _, u := range list {
		out[u.Name] = UnitState{Load: u.LoadState, Active: u.ActiveState, Sub: u.SubState}
	}
	return out, nil
}

// Action starts, stops or restarts a unit and waits for the job to finish
func (s *Systemd) Action(ctx context.Context, unitName, action string) error {
	if err := checkAction(action); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	c, err := s.get(ctx)
	if err != nil {
		return fmt.Errorf("%s %s: %w", action, unitName, err)
	}
	done := make(chan string, 1)
	switch action {
	case "start":
		_, err = c.StartUnitContext(ctx, unitName, "replace", done)
	case "stop":
		_, err = c.StopUnitContext(ctx, unitName, "replace", done)
	case "restart":
		_, err = c.RestartUnitContext(ctx, unitName, "replace", done)
	}
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s %s: %w; the request may already have reached systemd. Check its state and logs before retrying", action, unitName, ctx.Err())
		}
		if !c.Connected() {
			return fmt.Errorf("%s %s: %w; %w. Check its state and logs before retrying", action, unitName, err, errOutcomeUnknown)
		}
		return fmt.Errorf("%s %s: %w", action, unitName, err)
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case result := <-done:
			if result != "done" {
				return unitFailure(ctx, c, action, unitName, result)
			}
			return nil
		case <-ticker.C:
			if !c.Connected() {
				return fmt.Errorf("%s %s: connection to systemd was lost: %w. Check its state and logs before retrying", action, unitName, errOutcomeUnknown)
			}
		case <-ctx.Done():
			return fmt.Errorf("%s %s: %w; the systemd job may still be running. Check its state and logs before retrying", action, unitName, ctx.Err())
		}
	}
}

func unitFailure(ctx context.Context, conn systemdConnection, action, unit, result string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	props, _ := conn.GetUnitTypePropertiesContext(ctx, unit, "Service")
	detail := ""
	if serviceResult, ok := props["Result"].(string); ok && serviceResult != "" && serviceResult != "success" {
		detail = fmt.Sprintf("; service result: %s, exit status: %v", serviceResult, props["ExecMainStatus"])
	}
	if result == "timeout" {
		return fmt.Errorf("%s %s: systemd job timed out%s: %w (see the unit logs)", action, unit, detail, context.DeadlineExceeded)
	}
	return fmt.Errorf("%s %s: systemd job %s%s (see the unit logs)", action, unit, result, detail)
}

// Reload is `systemctl --user daemon-reload`, which reruns the Quadlet generator
func (s *Systemd) Reload(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := s.get(ctx)
	if err != nil {
		return err
	}
	if err := c.ReloadContext(ctx); err != nil {
		return fmt.Errorf("daemon-reload: %w", err)
	}
	return nil
}

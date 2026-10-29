package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/coreos/go-systemd/v22/dbus"
)

// Systemd talks to the user's systemd instance over D-Bus
type Systemd struct {
	mu   sync.Mutex
	conn *dbus.Conn
}

func (s *Systemd) get(ctx context.Context) (*dbus.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && s.conn.Connected() {
		return s.conn, nil
	}
	c, err := dbus.NewUserConnectionContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to the systemd user instance: %w", err)
	}
	s.conn = c
	return c, nil
}

type UnitState struct {
	Load   string `json:"load"`   // loaded, not-found, …
	Active string `json:"active"` // active, inactive, failed, activating, …
	Sub    string `json:"sub"`
}

func (s *Systemd) Status(ctx context.Context, units []string) (map[string]UnitState, error) {
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
	c, err := s.get(ctx)
	if err != nil {
		return err
	}
	done := make(chan string, 1)
	switch action {
	case "start":
		_, err = c.StartUnitContext(ctx, unitName, "replace", done)
	case "stop":
		_, err = c.StopUnitContext(ctx, unitName, "replace", done)
	case "restart":
		_, err = c.RestartUnitContext(ctx, unitName, "replace", done)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return err
	}
	select {
	case res := <-done:
		if res != "done" {
			return fmt.Errorf("%s %s: %s (see the logs)", action, unitName, res)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reload is `systemctl --user daemon-reload`, which reruns the Quadlet generator
func (s *Systemd) Reload(ctx context.Context) error {
	c, err := s.get(ctx)
	if err != nil {
		return err
	}
	return c.ReloadContext(ctx)
}

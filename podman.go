package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Pod     string            `json:"PodName"`
	Ports   json.RawMessage   `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
	Created json.RawMessage   `json:"Created"`
}

// Unit is the systemd unit Quadlet started this container from, if any
func (c Container) Unit() string {
	return c.Labels["PODMAN_SYSTEMD_UNIT"]
}

func (client *Podman) Containers(ctx context.Context) ([]Container, error) {
	var cs []Container
	err := client.json(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"true"}, "pod": {"true"}}, nil, &cs)
	return nonNil(cs), err
}

type Image struct {
	ID         string   `json:"Id"`
	Names      []string `json:"Names"`
	Size       int64    `json:"Size"`
	Created    int64    `json:"Created"`
	Containers int      `json:"Containers"`
	Dangling   bool     `json:"Dangling"`
}

func (client *Podman) Images(ctx context.Context) ([]Image, error) {
	var is []Image
	err := client.json(ctx, http.MethodGet, "/images/json", nil, nil, &is)
	return nonNil(is), err
}

func (client *Podman) PruneImages(ctx context.Context, all bool) (string, error) {
	var reports []struct {
		ID   string `json:"Id"`
		Err  json.RawMessage
		Size uint64
	}
	err := client.json(ctx, http.MethodPost, "/images/prune", url.Values{"all": {fmt.Sprint(all)}}, nil, &reports)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	var failures []error
	var reclaimed uint64
	for _, report := range reports {
		if len(report.Err) != 0 && string(report.Err) != "null" {
			failures = append(failures, fmt.Errorf("prune %s: %s", report.ID, report.Err))
			continue
		}
		fmt.Fprintln(&output, report.ID)
		reclaimed += report.Size
	}
	fmt.Fprintf(&output, "Reclaimed %d bytes", reclaimed)
	return output.String(), errors.Join(failures...)
}

type Network struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Internal bool   `json:"internal"`
	DNS      bool   `json:"dns_enabled"`
	Subnets  []struct {
		Subnet  string `json:"subnet"`
		Gateway string `json:"gateway"`
	} `json:"subnets"`
}

type Volume struct {
	Name       string `json:"Name"`
	Driver     string `json:"Driver"`
	Mountpoint string `json:"Mountpoint"`
	CreatedAt  string `json:"CreatedAt"`
}

func (client *Podman) Networks(ctx context.Context) ([]Network, error) {
	var ns []Network
	err := client.json(ctx, http.MethodGet, "/networks/json", nil, nil, &ns)
	return nonNil(ns), err
}

func (client *Podman) Volumes(ctx context.Context) ([]Volume, error) {
	var vs []Volume
	err := client.json(ctx, http.MethodGet, "/volumes/json", nil, nil, &vs)
	return nonNil(vs), err
}

var secretNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,252}$`)

const maxSecretSize = 512 * 1024 // Podman's limit

type Secret struct {
	ID        string `json:"ID"`
	CreatedAt string `json:"CreatedAt"`
	UpdatedAt string `json:"UpdatedAt"`
	Spec      struct {
		Name   string `json:"Name"`
		Driver struct {
			Name string `json:"Name"`
		} `json:"Driver"`
	} `json:"Spec"`
}

func (client *Podman) Secrets(ctx context.Context) ([]Secret, error) {
	var secrets []Secret
	err := client.json(ctx, http.MethodGet, "/secrets/json", nil, nil, &secrets)
	return nonNil(secrets), err
}

// SetSecret creates a secret, or replaces its value when replace is set
func (client *Podman) SetSecret(ctx context.Context, name string, value []byte, replace bool) error {
	if !secretNameRe.MatchString(name) {
		return errors.New("secret names use letters, digits, dot, dash and underscore")
	}
	if len(value) == 0 || len(value) > maxSecretSize {
		return errors.New("a secret value must be between 1 byte and 512 KiB")
	}
	var result struct{ ID string }
	if err := client.json(ctx, http.MethodPost, "/secrets/create", url.Values{"name": {name}, "replace": {fmt.Sprint(replace)}}, bytes.NewReader(value), &result); err != nil {
		return err
	}
	if result.ID == "" {
		return fmt.Errorf("Podman API returned no created secret ID: %w", errOutcomeUnknown)
	}
	return nil
}

func (client *Podman) RemoveSecret(ctx context.Context, name string) error {
	if !secretNameRe.MatchString(name) {
		return errors.New("invalid secret name")
	}
	return client.json(ctx, http.MethodDelete, "/secrets/"+url.PathEscape(name), nil, nil, nil)
}

// Convert turns a `docker run`/`podman run` command or a compose file into Quadlet files using podlet
func Convert(ctx context.Context, bin, input string) (string, error) {
	if bin == "" {
		return "", errors.New("podlet is not installed")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	input = strings.TrimSpace(input)
	var cmd *exec.Cmd
	if fields := strings.Fields(input); len(fields) > 1 && (fields[0] == "docker" || fields[0] == "podman") {
		// podlet podman run …; the command line is split like a shell would for the common cases
		args, err := shellSplit(input)
		if err != nil {
			return "", err
		}
		args[0] = "podman"
		cmd = exec.CommandContext(ctx, bin, args...)
	} else {
		cmd = exec.CommandContext(ctx, bin, "compose", "-")
		cmd.Stdin = strings.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("podlet: %s", firstNonEmpty(strings.TrimSpace(stderr.String()), err.Error()))
	}
	return stdout.String(), nil
}

// shellSplit splits a command line, honouring single and double quotes and backslash line continuations
func shellSplit(s string) ([]string, error) {
	s = strings.ReplaceAll(s, "\\\n", " ")
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

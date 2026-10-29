package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Podman calls are made through the CLI's JSON output, which is stable across versions
// and needs no API socket.

func podman(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "podman", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("podman %s: %s", args[0], msg)
	}
	return stdout.Bytes(), nil
}

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

func Containers(ctx context.Context) ([]Container, error) {
	out, err := podman(ctx, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	var cs []Container
	return cs, json.Unmarshal(out, &cs)
}

type Image struct {
	ID         string   `json:"Id"`
	Names      []string `json:"Names"`
	Size       int64    `json:"Size"`
	Created    int64    `json:"Created"`
	Containers int      `json:"Containers"`
	Dangling   bool     `json:"Dangling"`
}

func Images(ctx context.Context) ([]Image, error) {
	out, err := podman(ctx, "images", "--format", "json")
	if err != nil {
		return nil, err
	}
	var is []Image
	return is, json.Unmarshal(out, &is)
}

func PruneImages(ctx context.Context, all bool) (string, error) {
	args := []string{"image", "prune", "--force"}
	if all {
		args = append(args, "--all")
	}
	out, err := podman(ctx, args...)
	return string(out), err
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

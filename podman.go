package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Podman calls are made through the CLI's JSON output, which is stable across versions
// and needs no API socket.

func podman(ctx context.Context, args ...string) ([]byte, error) {
	return podmanIn(ctx, nil, args...)
}

// podmanIn runs podman with stdin, which is how secret values are passed: never as arguments
func podmanIn(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "podman", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
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

func Networks(ctx context.Context) ([]Network, error) {
	out, err := podman(ctx, "network", "ls", "--format", "json")
	if err != nil {
		return nil, err
	}
	var ns []Network
	return ns, json.Unmarshal(out, &ns)
}

func Volumes(ctx context.Context) ([]Volume, error) {
	out, err := podman(ctx, "volume", "ls", "--format", "json")
	if err != nil {
		return nil, err
	}
	var vs []Volume
	return vs, json.Unmarshal(out, &vs)
}

/* Secrets: values go in over stdin and never come back out */

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

// Secrets lists the secrets. `podman secret ls --format json` prints "json" in Podman 5.4 and ls
// humanizes its dates, so the IDs come from ls and the details from inspect, which leaves
// the value out unless --showsecret is given.
func Secrets(ctx context.Context) ([]Secret, error) {
	out, err := podman(ctx, "secret", "ls", "--noheading", "--format", "{{.ID}}")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	ss := []Secret{}
	if len(ids) == 0 {
		return ss, nil
	}
	out, err = podman(ctx, append([]string{"secret", "inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	return ss, json.Unmarshal(out, &ss)
}

// SetSecret creates a secret, or replaces its value when replace is set
func SetSecret(ctx context.Context, name string, value []byte, replace bool) error {
	if !secretNameRe.MatchString(name) {
		return errors.New("secret names use letters, digits, dot, dash and underscore")
	}
	if len(value) == 0 || len(value) > maxSecretSize {
		return errors.New("a secret value must be between 1 byte and 512 KiB")
	}
	args := []string{"secret", "create"}
	if replace {
		args = append(args, "--replace")
	}
	_, err := podmanIn(ctx, value, append(args, name, "-")...)
	return err
}

func RemoveSecret(ctx context.Context, name string) error {
	if !secretNameRe.MatchString(name) {
		return errors.New("invalid secret name")
	}
	_, err := podman(ctx, "secret", "rm", name)
	return err
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

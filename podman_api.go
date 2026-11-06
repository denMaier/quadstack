package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const podmanAPIPath = "/v4.0.0/libpod"

type Podman struct {
	socket string
	client *http.Client
}

func newPodman(socket string) *Podman {
	if socket == "" {
		socket = defaultPodmanSocket()
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &Podman{socket: socket, client: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
			ResponseHeaderTimeout: 15 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   8,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (client *Podman) request(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	endpoint := "http://podman" + podmanAPIPath + path
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func (client *Podman) send(req *http.Request) (*http.Response, error) {
	response, err := client.client.Do(req)
	if err != nil {
		if req.Method != http.MethodGet {
			err = fmt.Errorf("%w; %w; check state before retrying", err, errOutcomeUnknown)
		}
		return nil, fmt.Errorf("Podman API %s %s via %s: %w. Ensure podman.socket is enabled for the Quadlet user", req.Method, req.URL.Path, client.socket, err)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 || response.StatusCode == http.StatusSwitchingProtocols {
		if response.StatusCode == http.StatusSwitchingProtocols && req.Header.Get("Upgrade") == "" {
			response.Body.Close()
			return nil, errors.New("Podman API unexpectedly upgraded a non-streaming request")
		}
		return response, nil
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	var failure struct{ Message string }
	json.Unmarshal(data, &failure)
	message := firstNonEmpty(failure.Message, strings.TrimSpace(string(data)), response.Status)
	return nil, fmt.Errorf("Podman API %s %s: HTTP %d: %s", req.Method, req.URL.Path, response.StatusCode, message)
}

func (client *Podman) json(ctx context.Context, method, path string, query url.Values, body io.Reader, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := client.request(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	response, err := client.send(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if result == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(response.Body, 32<<20))
		return err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil {
		return fmt.Errorf("reading Podman API %s: %w", path, err)
	}
	if len(data) > 32<<20 {
		return fmt.Errorf("Podman API %s response exceeds 32 MiB", path)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decoding Podman API %s: %w", path, err)
	}
	return nil
}

func (client *Podman) Pull(ctx context.Context, image string, progress io.Writer) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	req, err := client.request(ctx, http.MethodPost, "/images/pull", url.Values{"reference": {image}}, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.send(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(response.Body)
	var images []string
	for {
		var report struct {
			Stream string   `json:"stream"`
			Error  string   `json:"error"`
			Images []string `json:"images"`
			ID     string   `json:"id"`
		}
		if err := decoder.Decode(&report); err != nil {
			if errors.Is(err, io.EOF) && len(images) > 0 {
				return images, nil
			}
			return nil, fmt.Errorf("pull %s: progress stream ended before successful completion: %w; %w", image, err, errOutcomeUnknown)
		}
		if report.Error != "" {
			return nil, fmt.Errorf("pull %s: %s", image, report.Error)
		}
		if report.Stream != "" && progress != nil {
			if _, err := io.WriteString(progress, report.Stream); err != nil {
				return nil, err
			}
		}
		if len(report.Images) > 0 {
			images = report.Images
		} else if report.ID != "" {
			images = []string{report.ID}
		}
	}
}

type podmanExec struct {
	ID     string
	Tty    bool
	podman *Podman
}

func (client *Podman) CreateExec(ctx context.Context, container string, command []string, tty bool) (*podmanExec, error) {
	body, err := json.Marshal(struct {
		Cmd          []string
		Tty          bool
		AttachStdin  bool
		AttachStdout bool
		AttachStderr bool
	}{command, tty, tty, true, true})
	if err != nil {
		return nil, err
	}
	var result struct{ ID string }
	if err := client.json(ctx, http.MethodPost, "/containers/"+url.PathEscape(container)+"/exec", nil, bytes.NewReader(body), &result); err != nil {
		return nil, err
	}
	if result.ID == "" {
		return nil, errors.New("Podman API returned an empty exec session ID")
	}
	return &podmanExec{ID: result.ID, Tty: tty, podman: client}, nil
}

type execStream struct {
	io.ReadWriteCloser
	stop func() bool
}

func (stream *execStream) Close() error {
	stream.stop()
	return stream.ReadWriteCloser.Close()
}

func (session *podmanExec) Start(ctx context.Context) (io.ReadWriteCloser, error) {
	body, err := json.Marshal(struct{ Detach, Tty bool }{false, session.Tty})
	if err != nil {
		return nil, err
	}
	req, err := session.podman.request(ctx, http.MethodPost, "/exec/"+url.PathEscape(session.ID)+"/start", nil, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	response, err := session.podman.send(req)
	if err != nil {
		return nil, err
	}
	stream, ok := response.Body.(io.ReadWriteCloser)
	if !ok || response.StatusCode != http.StatusSwitchingProtocols {
		response.Body.Close()
		return nil, fmt.Errorf("Podman API did not upgrade the exec connection: %w", errOutcomeUnknown)
	}
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	return &execStream{ReadWriteCloser: stream, stop: stop}, nil
}

func (session *podmanExec) ExitCode(ctx context.Context) (int, error) {
	var result struct {
		Running  bool
		ExitCode *int
	}
	if err := session.podman.json(ctx, http.MethodGet, "/exec/"+url.PathEscape(session.ID)+"/json", nil, nil, &result); err != nil {
		return -1, err
	}
	if result.Running {
		return -1, fmt.Errorf("exec stream closed while the command is still running: %w", errOutcomeUnknown)
	}
	if result.ExitCode == nil {
		return -1, fmt.Errorf("Podman API returned no exec exit code: %w", errOutcomeUnknown)
	}
	return *result.ExitCode, nil
}

func (session *podmanExec) Resize(ctx context.Context, cols, rows uint16) error {
	return session.podman.json(ctx, http.MethodPost, "/exec/"+url.PathEscape(session.ID)+"/resize", url.Values{"w": {strconv.Itoa(int(cols))}, "h": {strconv.Itoa(int(rows))}}, nil, nil)
}

func copyExecOutput(reader io.Reader, stdout, stderr io.Writer) error {
	var header [8]byte
	for {
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if header[1] != 0 || header[2] != 0 || header[3] != 0 {
			return errors.New("invalid Podman exec stream header")
		}
		output := stdout
		switch header[0] {
		case 0, 1:
		case 2:
			output = stderr
		case 3:
			return errors.New("Podman reported an exec stream error")
		default:
			return errors.New("unknown Podman exec stream type")
		}
		if _, err := io.CopyN(output, reader, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			return err
		}
	}
}

func (client *Podman) Exec(ctx context.Context, container string, command []string, stdout, stderr io.Writer) (int, error) {
	session, err := client.CreateExec(ctx, container, command, false)
	if err != nil {
		return -1, err
	}
	stream, err := session.Start(ctx)
	if err != nil {
		return -1, err
	}
	defer stream.Close()
	if err := copyExecOutput(stream, stdout, stderr); err != nil {
		if ctx.Err() != nil {
			return -1, fmt.Errorf("exec observation stopped: %w; the command may still be running", ctx.Err())
		}
		return -1, err
	}
	return session.ExitCode(ctx)
}

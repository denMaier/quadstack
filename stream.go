package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"
)

var (
	unitRe      = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]+\.service$`)
	containerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// The websockets talk to an xterm.js client: binary frames carry terminal data both ways, the
// client sends {"cols","rows"} text frames on resize, and the server ends with an
// {"exit": code, "error": …} text frame.

func exitFrame(code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"exit": code, "error": msg})
	return b
}

func (s *Server) originPatterns() []string {
	if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Host != "" {
		return []string{u.Host}
	}
	return nil
}

var errClientGone = errors.New("the browser connection closed")

// relay sends what src produces to the client as binary frames. It returns nil when src ends,
// src's error, or errClientGone when the client can no longer be written to.
func relay(ctx context.Context, c *websocket.Conn, src io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if c.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
				return errClientGone
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// GET /ws/logs?unit=a.service&unit=b.service follows the journal of the units
func (s *Server) wsLogs(w http.ResponseWriter, r *http.Request) {
	units := r.URL.Query()["unit"]
	if len(units) == 0 {
		http.Error(w, "no unit", http.StatusBadRequest)
		return
	}
	args := []string{"--user", "--follow", "--lines=300", "--no-pager", "--output=short-iso"}
	for _, u := range units {
		if !unitRe.MatchString(u) {
			http.Error(w, "invalid unit", http.StatusBadRequest)
			return
		}
		args = append(args, "--unit="+u)
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.originPatterns()})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// A log needs no input or terminal size; reading only notices when the client leaves
	go func() {
		defer cancel()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}()

	out, pipe, err := os.Pipe()
	if err != nil {
		c.Write(ctx, websocket.MessageText, exitFrame(-1, err.Error()))
		return
	}
	defer out.Close()
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	cmd.Env = append(os.Environ(), "SYSTEMD_COLORS=1")
	cmd.Stdout, cmd.Stderr = pipe, pipe
	err = cmd.Start()
	pipe.Close()
	if err != nil {
		c.Write(ctx, websocket.MessageText, exitFrame(-1, err.Error()))
		return
	}
	if relay(ctx, c, out) == errClientGone {
		cmd.Process.Kill()
		cmd.Wait()
		return
	}
	code := 0
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	c.Write(ctx, websocket.MessageText, exitFrame(code, ""))
	c.Close(websocket.StatusNormalClosure, "")
}

func stackImages(stack *Stack) []string {
	var images []string
	for _, file := range stack.Files {
		if file.Image != "" && !strings.HasSuffix(file.Image, ".image") && !strings.HasSuffix(file.Image, ".build") && !slices.Contains(images, file.Image) {
			images = append(images, file.Image)
		}
	}
	return images
}

func (s *Server) pullImages(w http.ResponseWriter, r *http.Request) {
	var req struct{ Stack string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := FindStack(s.cfg.QuadletDir, req.Stack)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("unknown stack"))
		return
	}
	images := stackImages(st)
	if len(images) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("this stack has no images to pull"))
		return
	}
	var tasks []operationTask
	for _, image := range images {
		tasks = append(tasks, operationTask{name: "Pull " + image, timeout: 15 * time.Minute, run: func(ctx context.Context, report func(string)) error {
			progress := &progressWriter{buffer: boundedBuffer{limit: 16 << 10}, report: report}
			ids, err := s.podman.Pull(ctx, image, progress)
			if err == nil {
				fmt.Fprintf(progress, "\nPulled %s\n", strings.Join(ids, ", "))
			}
			return err
		}})
	}
	s.enqueue(w, "Pull images for "+st.Name, st.ID, tasks, false)
}

// GET /ws/exec?container=<name> opens a shell inside a container
func (s *Server) wsExec(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.Terminal {
		http.Error(w, "the terminal is disabled (QUADSTACK_TERMINAL=false)", http.StatusForbidden)
		return
	}
	name := r.URL.Query().Get("container")
	if !containerRe.MatchString(name) {
		http.Error(w, "invalid container", http.StatusBadRequest)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.originPatterns()})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	fail := func(err error) { c.Write(ctx, websocket.MessageText, exitFrame(-1, err.Error())) }
	shell := "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"
	session, err := s.podman.CreateExec(ctx, name, []string{"sh", "-c", shell}, true)
	if err != nil {
		fail(err)
		return
	}
	stream, err := session.Start(ctx)
	if err != nil {
		fail(err)
		return
	}
	defer stream.Close()
	go func() {
		defer cancel()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageText {
				var size struct{ Cols, Rows uint16 }
				if json.Unmarshal(data, &size) == nil && size.Cols > 0 && size.Rows > 0 {
					resizeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
					err := session.Resize(resizeCtx, size.Cols, size.Rows)
					stop()
					if err != nil {
						c.Write(ctx, websocket.MessageText, []byte(`{"error":"Could not resize the container terminal"}`))
					}
				}
			} else if _, err := stream.Write(data); err != nil {
				return
			}
		}
	}()
	if err := relay(ctx, c, stream); err != nil {
		if err != errClientGone && ctx.Err() == nil {
			fail(err)
		}
		return
	}
	code, err := session.ExitCode(ctx)
	if err != nil {
		fail(err)
		return
	}
	c.Write(ctx, websocket.MessageText, exitFrame(code, ""))
	c.Close(websocket.StatusNormalClosure, "")
}

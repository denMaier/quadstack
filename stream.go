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
	"github.com/creack/pty"
)

var (
	unitRe      = regexp.MustCompile(`^[A-Za-z0-9@._:\\-]+\.service$`)
	containerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// streamPTY runs cmd in a pseudo terminal and connects it to an xterm.js client.
// Binary frames carry terminal data both ways; the client sends {"cols","rows"} text
// frames on resize and the server ends with an {"exit": code} text frame.
func (s *Server) streamPTY(w http.ResponseWriter, r *http.Request, cmd *exec.Cmd, interactive bool) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.originPatterns()})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "SYSTEMD_COLORS=1")
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 30})
	if err != nil {
		c.Write(ctx, websocket.MessageText, exitFrame(-1, err.Error()))
		return
	}
	defer func() {
		tty.Close()
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()
	stopProcess := context.AfterFunc(ctx, func() {
		tty.Close()
		cmd.Process.Kill()
	})
	defer stopProcess()

	// client -> process
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
					pty.Setsize(tty, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
				}
				continue
			}
			if interactive {
				tty.Write(data)
			}
		}
	}()

	// process -> client
	buf := make([]byte, 32*1024)
	for {
		n, err := tty.Read(buf)
		if n > 0 {
			if c.Write(ctx, websocket.MessageBinary, buf[:n]) != nil {
				return
			}
		}
		if err != nil {
			break
		}
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

// GET /ws/logs?unit=a.service&unit=b.service
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
	s.streamPTY(w, r, exec.Command("journalctl", args...), false)
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

func (server *Server) pullImages(writer http.ResponseWriter, request *http.Request) {
	var req struct{ Stack string }
	if err := readJSON(request, &req); err != nil {
		writeErr(writer, http.StatusBadRequest, err)
		return
	}
	st, err := FindStack(server.cfg.QuadletDir, req.Stack)
	if err != nil {
		writeErr(writer, http.StatusNotFound, errors.New("unknown stack"))
		return
	}
	images := stackImages(st)
	if len(images) == 0 {
		writeErr(writer, http.StatusBadRequest, errors.New("this stack has no images to pull"))
		return
	}
	var tasks []operationTask
	for _, image := range images {
		tasks = append(tasks, operationTask{name: "Pull " + image, timeout: 15 * time.Minute, run: func(ctx context.Context, report func(string)) error {
			writer := &progressWriter{buffer: boundedBuffer{limit: 16 << 10}, report: report}
			ids, err := server.podman.Pull(ctx, image, writer)
			if err == nil {
				fmt.Fprintf(writer, "\nPulled %s\n", strings.Join(ids, ", "))
			}
			return err
		}})
	}
	server.enqueue(writer, "Pull images for "+st.Name, st.ID, tasks, false)
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
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.originPatterns()})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	fail := func(err error) { connection.Write(ctx, websocket.MessageText, exitFrame(-1, err.Error())) }
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
			typ, data, err := connection.Read(ctx)
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
						connection.Write(ctx, websocket.MessageText, []byte(`{"error":"Could not resize the container terminal"}`))
					}
				}
			} else if _, err := stream.Write(data); err != nil {
				return
			}
		}
	}()
	buffer := make([]byte, 32<<10)
	for {
		size, readErr := stream.Read(buffer)
		if size > 0 {
			if err := connection.Write(ctx, websocket.MessageBinary, buffer[:size]); err != nil {
				return
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && ctx.Err() == nil {
				fail(readErr)
				return
			}
			break
		}
	}
	code, err := session.ExitCode(ctx)
	if err != nil {
		fail(err)
		return
	}
	connection.Write(ctx, websocket.MessageText, exitFrame(code, ""))
	connection.Close(websocket.StatusNormalClosure, "")
}

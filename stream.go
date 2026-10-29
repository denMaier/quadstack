package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"

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

// GET /ws/pull?stack=<id> pulls the images of a stack's containers
func (s *Server) wsPull(w http.ResponseWriter, r *http.Request) {
	st, err := FindStack(s.cfg.QuadletDir, r.URL.Query().Get("stack"))
	if err != nil {
		http.Error(w, "unknown stack", http.StatusNotFound)
		return
	}
	var images []string
	for _, f := range st.Files {
		// .image and .build references are handled by their own units
		if f.Image != "" && !strings.HasSuffix(f.Image, ".image") && !strings.HasSuffix(f.Image, ".build") {
			images = append(images, f.Image)
		}
	}
	if len(images) == 0 {
		http.Error(w, "this stack has no images to pull", http.StatusBadRequest)
		return
	}
	// One shell so the output reads like a terminal session; images are passed as arguments, not interpolated
	script := `for i in "$@"; do printf '\033[1m$ podman pull %s\033[0m\n' "$i"; podman pull "$i" || exit $?; done`
	s.streamPTY(w, r, exec.Command("sh", append([]string{"-c", script, "pull"}, images...)...), false)
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
	shell := "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"
	s.streamPTY(w, r, exec.Command("podman", "exec", "-it", name, "sh", "-c", shell), true)
}

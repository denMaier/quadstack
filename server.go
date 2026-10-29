package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

type Server struct {
	cfg     *Config
	auth    *Authenticator
	files   *Files
	systemd *Systemd
	static  fs.FS
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", s.auth.HandleLogin)
	mux.HandleFunc("GET /auth/callback", s.auth.HandleCallback)
	mux.HandleFunc("GET /auth/logout", s.auth.HandleLogout)

	api := map[string]http.HandlerFunc{
		"GET /api/me":                s.me,
		"GET /api/stacks":            s.stacks,
		"POST /api/stacks":           s.createStack,
		"DELETE /api/stacks/{id...}": s.removeStack,
		"POST /api/stack-action":     s.stackAction,
		"POST /api/unit-action":      s.unitAction,
		"POST /api/reload":           s.reload,
		"GET /api/file":              s.readFile,
		"PUT /api/file":              s.writeFile,
		"POST /api/file":             s.createFile,
		"DELETE /api/file":           s.deleteFile,
		"POST /api/file/move":        s.moveFile,
		"POST /api/validate":         s.validate,
		"GET /api/history":           s.history,
		"GET /api/history/version":   s.historyVersion,
		"GET /api/images":            s.images,
		"POST /api/images/prune":     s.pruneImages,
		"POST /api/convert":          s.convert,
		"GET /ws/logs":               s.wsLogs,
		"GET /ws/pull":               s.wsPull,
		"GET /ws/exec":               s.wsExec,
	}
	for pattern, h := range api {
		mux.Handle(pattern, s.requireSession(h))
	}

	files := http.FileServerFS(s.static)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.auth.Session(r); !ok {
			http.Redirect(w, r, "/auth/login?next="+r.URL.EscapedPath(), http.StatusFound)
			return
		}
		// Single page app: unknown paths get index.html
		if _, err := fs.Stat(s.static, strings.TrimPrefix(path.Clean(r.URL.Path), "/")); err != nil {
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// requireSession guards the API. Changing requests must carry the X-Quadstack header, which
// a cross-site form cannot set, as CSRF protection on top of the SameSite cookie.
func (s *Server) requireSession(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.auth.Session(r); !ok {
			writeErr(w, http.StatusUnauthorized, errors.New("not signed in"))
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Quadstack") != "1" {
			writeErr(w, http.StatusForbidden, errors.New("missing X-Quadstack header"))
			return
		}
		next(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	writeJSONStatus(w, http.StatusOK, v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSONStatus(w, status, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

func relParam(w http.ResponseWriter, p string) (string, bool) {
	rel, err := cleanRel(p)
	if err == nil && !isManaged(path.Base(rel)) {
		err = errors.New("not a Quadlet, drop-in (.conf) or env file")
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return "", false
	}
	return rel, true
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	sess, _ := s.auth.Session(r)
	host, _ := os.Hostname()
	writeJSON(w, map[string]any{
		"name":       sess.Name,
		"host":       host,
		"quadletDir": s.cfg.QuadletDir,
		"validation": s.cfg.QuadletBin != "",
		"podlet":     s.cfg.PodletBin != "",
		"terminal":   s.cfg.Terminal,
		"journal":    journalProblem(r.Context()),
	})
}

// journalProblem explains why this account cannot read its logs, if it cannot. journald gives
// only accounts with UID >= 1000 their own journal files; a system account's logs land in the
// system journal, which it may not read.
func journalProblem(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "journalctl", "--user", "--lines=0", "--no-pager", "--quiet").CombinedOutput()
	if strings.Contains(string(out), "insufficient permissions") {
		return "This account cannot read its logs: as a system account (UID below 1000) its journal is the system journal. " +
			"See \"Logs of system accounts\" in the README for a read-only ACL that fixes this."
	}
	return ""
}

type stackView struct {
	*Stack
	Units      map[string]UnitState `json:"units"`
	Containers []Container          `json:"containers"`
	State      string               `json:"state"` // running, partial, stopped, failed, empty
}

func (s *Server) stacks(w http.ResponseWriter, r *http.Request) {
	inv, err := Scan(s.cfg.QuadletDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stacks := inv.Stacks
	var units []string
	for _, st := range stacks {
		for _, f := range st.Files {
			if f.Unit != "" {
				units = append(units, f.Unit)
			}
		}
	}
	for _, r := range inv.Resources {
		if r.Unit != "" && r.Stack == "" {
			units = append(units, r.Unit)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Missing systemd or Podman (e.g. while developing) degrades the view instead of failing it
	var warnings []string
	status, err := s.systemd.Status(ctx, units)
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	containers, err := Containers(ctx)
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	byUnit := map[string][]Container{}
	for _, c := range containers {
		byUnit[c.Unit()] = append(byUnit[c.Unit()], c)
	}

	views := make([]stackView, 0, len(stacks))
	for _, st := range stacks {
		v := stackView{Stack: st, Units: map[string]UnitState{}, Containers: []Container{}}
		for _, f := range st.Files {
			if f.Unit != "" {
				v.Units[f.Unit] = status[f.Unit]
				v.Containers = append(v.Containers, byUnit[f.Unit]...)
			}
		}
		v.State = stackState(st.Units(), v.Units)
		views = append(views, v)
	}
	resources := make([]map[string]any, 0, len(inv.Resources))
	for _, r := range inv.Resources {
		resources = append(resources, map[string]any{"file": r.QFile, "stack": r.Stack, "usedBy": r.UsedBy, "state": status[r.Unit]})
	}
	writeJSON(w, map[string]any{"stacks": views, "resources": resources, "warnings": warnings})
}

func stackState(units []string, status map[string]UnitState) string {
	if len(units) == 0 {
		return "empty"
	}
	active, failed := 0, 0
	for _, u := range units {
		switch status[u].Active {
		case "active", "activating", "reloading":
			active++
		case "failed":
			failed++
		}
	}
	switch {
	case failed > 0:
		return "failed"
	case active == len(units):
		return "running"
	case active > 0:
		return "partial"
	}
	return "stopped"
}

func (s *Server) createStack(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.files.CreateStack(strings.TrimSpace(req.Name)); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"id": "d/" + strings.TrimSpace(req.Name)})
}

func (s *Server) removeStack(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name, ok := strings.CutPrefix(id, "d/")
	if !ok || strings.Contains(name, "/") {
		writeErr(w, http.StatusBadRequest, errors.New("only stack folders can be removed"))
		return
	}
	if err := s.files.RemoveStack(name); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("the stack folder must be empty"))
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// POST /api/stack-action {stack, action}: start, stop or restart every unit of a stack
func (s *Server) stackAction(w http.ResponseWriter, r *http.Request) {
	var req struct{ Stack, Action string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	st, err := FindStack(s.cfg.QuadletDir, req.Stack)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("unknown stack"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	var errs []error
	for _, u := range st.Units() {
		if err := s.systemd.Action(ctx, u, req.Action); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// POST /api/unit-action {unit, action}
func (s *Server) unitAction(w http.ResponseWriter, r *http.Request) {
	var req struct{ Unit, Action string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !unitRe.MatchString(req.Unit) {
		writeErr(w, http.StatusBadRequest, errors.New("invalid unit"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := s.systemd.Action(ctx, req.Unit, req.Action); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	if err := s.systemd.Reload(r.Context()); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	rel, ok := relParam(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	content, err := s.files.Read(rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, map[string]string{"path": rel, "content": content})
}

type writeReq struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	// What to do after saving: "" (only save), "reload" (daemon-reload) or
	// "restart" (daemon-reload, then restart the file's unit, like Dockge's Deploy)
	Apply string `json:"apply"`
	Force bool   `json:"force"` // save even when validation fails
}

// saveAndApply validates, writes and applies a file; created is false for updates
func (s *Server) saveAndApply(w http.ResponseWriter, r *http.Request, created bool) {
	var req writeReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rel, ok := relParam(w, req.Path)
	if !ok {
		return
	}
	v := s.files.Validate(r.Context(), rel, &req.Content)
	if !v.OK && !req.Force {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": "Validation failed", "validation": v})
		return
	}
	write := s.files.Write
	if created {
		write = s.files.Create
	}
	if err := write(rel, req.Content); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	resp := map[string]any{"ok": true, "validation": v}
	if req.Apply == "reload" || req.Apply == "restart" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		if err := s.systemd.Reload(ctx); err != nil {
			resp["applyError"] = err.Error()
		} else if f := parseFile(s.files.abs(rel), rel); req.Apply == "restart" && f.Unit != "" {
			if err := s.systemd.Action(ctx, f.Unit, "restart"); err != nil {
				resp["applyError"] = err.Error()
			}
		}
	}
	writeJSON(w, resp)
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request)  { s.saveAndApply(w, r, false) }
func (s *Server) createFile(w http.ResponseWriter, r *http.Request) { s.saveAndApply(w, r, true) }

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	rel, ok := relParam(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	// Stop the unit first so systemd does not keep a running service whose file is gone
	if f := parseFile(s.files.abs(rel), rel); f.Unit != "" && r.URL.Query().Get("stop") == "1" {
		s.systemd.Action(r.Context(), f.Unit, "stop")
	}
	if err := s.files.Delete(rel); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.systemd.Reload(r.Context())
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) moveFile(w http.ResponseWriter, r *http.Request) {
	var req struct{ From, To string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	from, ok := relParam(w, req.From)
	if !ok {
		return
	}
	to, ok := relParam(w, req.To)
	if !ok {
		return
	}
	if err := s.files.Move(from, to); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.systemd.Reload(r.Context())
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	var req writeReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rel, ok := relParam(w, req.Path)
	if !ok {
		return
	}
	writeJSON(w, s.files.Validate(r.Context(), rel, &req.Content))
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	rel, ok := relParam(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	vs, err := s.files.History(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, vs)
}

func (s *Server) historyVersion(w http.ResponseWriter, r *http.Request) {
	rel, ok := relParam(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	content, err := s.files.ReadVersion(rel, r.URL.Query().Get("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, map[string]string{"content": content})
}

func (s *Server) images(w http.ResponseWriter, r *http.Request) {
	is, err := Images(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, is)
}

func (s *Server) pruneImages(w http.ResponseWriter, r *http.Request) {
	var req struct{ All bool }
	readJSON(r, &req)
	out, err := PruneImages(r.Context(), req.All)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]string{"output": out})
}

func (s *Server) convert(w http.ResponseWriter, r *http.Request) {
	var req struct{ Input string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := Convert(r.Context(), s.cfg.PodletBin, req.Input)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]string{"output": out})
}

// GET /api/podman-resources lists the networks and volumes Podman knows, Quadlet-defined or not
func (s *Server) podmanResources(w http.ResponseWriter, r *http.Request) {
	ns, err := Networks(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	vs, err := Volumes(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]any{"networks": ns, "volumes": vs})
}

type secretUse struct {
	Stack  string `json:"stack"`
	Path   string `json:"path"`
	Type   string `json:"type"`
	Target string `json:"target"`
}

// secretUses maps secret names to the Quadlet files that reference them
func (s *Server) secretUses() (map[string][]secretUse, error) {
	inv, err := Scan(s.cfg.QuadletDir)
	if err != nil {
		return nil, err
	}
	uses := map[string][]secretUse{}
	for _, st := range inv.Stacks {
		for _, f := range st.Files {
			for _, ref := range f.Secrets {
				uses[ref.Name] = append(uses[ref.Name], secretUse{st.ID, f.Path, ref.Type, ref.Target})
			}
		}
	}
	return uses, nil
}

func (s *Server) secrets(w http.ResponseWriter, r *http.Request) {
	ss, err := Secrets(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	uses, err := s.secretUses()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, sec := range ss {
		seen[sec.Spec.Name] = true
		out = append(out, map[string]any{
			"name": sec.Spec.Name, "driver": sec.Spec.Driver.Name, "created": sec.CreatedAt, "updated": sec.UpdatedAt,
			"usedBy": nonNil(uses[sec.Spec.Name]), "exists": true,
		})
	}
	// Referenced in a file but missing in Podman: the unit will fail to start
	for name, u := range uses {
		if !seen[name] {
			out = append(out, map[string]any{"name": name, "usedBy": u, "exists": false})
		}
	}
	writeJSON(w, out)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// POST /api/secrets {name, value | fromFile, replace}. fromFile reads a file the service
// account can read, to turn a bind-mounted key file into a secret.
func (s *Server) setSecret(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Value    string `json:"value"`
		FromFile string `json:"fromFile"`
		Replace  bool   `json:"replace"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	value := []byte(req.Value)
	if req.FromFile != "" {
		b, err := readSecretFile(req.FromFile)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		value = b
	}
	if err := SetSecret(r.Context(), req.Name, value, req.Replace); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func readSecretFile(p string) ([]byte, error) {
	// Quadlet's %h is the service account's home
	if rest, ok := strings.CutPrefix(p, "%h"); ok {
		home, _ := os.UserHomeDir()
		p = home + rest
	}
	if !path.IsAbs(p) {
		return nil, errors.New("use an absolute path")
	}
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxSecretSize {
		return nil, errors.New("not a regular file of at most 512 KiB")
	}
	return os.ReadFile(p)
}

func (s *Server) removeSecret(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if uses, err := s.secretUses(); err == nil && len(uses[name]) > 0 && r.URL.Query().Get("force") != "1" {
		writeErr(w, http.StatusConflict, errors.New("this secret is still used by "+uses[name][0].Path))
		return
	}
	if err := RemoveSecret(r.Context(), name); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

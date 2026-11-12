package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	cfg        *Config
	auth       *Authenticator
	files      *Files
	systemd    systemdClient
	podman     *Podman
	operations Operations
	static     fs.FS
	grants     *Grants
	audit      *Audit
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", s.auth.HandleLogin)
	mux.HandleFunc("GET /auth/callback", s.auth.HandleCallback)
	mux.HandleFunc("GET /auth/logout", s.auth.HandleLogout)

	api := map[string]http.HandlerFunc{
		"GET /api/me":                   s.me,
		"GET /api/stacks":               s.stacks,
		"POST /api/stacks":              s.createStack,
		"DELETE /api/stacks/{id...}":    s.removeStack,
		"POST /api/stack-action":        s.stackAction,
		"POST /api/unit-action":         s.unitAction,
		"POST /api/reload":              s.reload,
		"POST /api/pull":                s.pullImages,
		"GET /api/operations":           s.listOperations,
		"GET /api/operations/{id}":      s.getOperation,
		"GET /api/file":                 s.readFile,
		"PUT /api/file":                 s.writeFile,
		"POST /api/file":                s.createFile,
		"DELETE /api/file":              s.deleteFile,
		"POST /api/file/move":           s.moveFile,
		"POST /api/validate":            s.validate,
		"GET /api/history":              s.history,
		"GET /api/history/version":      s.historyVersion,
		"GET /api/history/deleted":      s.deletedFiles,
		"DELETE /api/history":           s.forgetHistory,
		"GET /api/images":               s.images,
		"POST /api/images/prune":        s.pruneImages,
		"POST /api/convert":             s.convert,
		"GET /api/podman-resources":     s.podmanResources,
		"GET /api/secrets":              s.secrets,
		"POST /api/secrets":             s.setSecret,
		"DELETE /api/secrets":           s.removeSecret,
		"GET /api/mcp":                  s.mcpInfo,
		"POST /api/mcp/grants":          s.createGrant,
		"PUT /api/mcp/grants/{id}":      s.updateGrant,
		"POST /api/mcp/grants/{id}/end": s.endGrant,
		"DELETE /api/mcp/grants/{id}":   s.deleteGrant,
		"GET /ws/logs":                  s.wsLogs,
		"GET /ws/exec":                  s.wsExec,
	}
	for pattern, h := range api {
		mux.Handle(pattern, s.requireSession(h))
	}

	// MCP clients authenticate with a grant's bearer token instead of the session cookie
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		c, ok := callerFrom(r.Context())
		if !ok {
			return nil
		}
		return s.mcpServer(c.grant, c.from)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	for _, m := range []string{"GET", "POST", "DELETE"} {
		mux.Handle(m+" /mcp", s.mcpEndpoint(mcpHandler))
	}

	files := http.FileServerFS(s.static)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// Unknown API paths must not fall through to the single page app's index.html
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws/") {
			writeErr(w, http.StatusNotFound, errors.New("no such endpoint: "+r.URL.Path))
			return
		}
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
		"version":    version,
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
	o, err := s.overview(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	resources := make([]map[string]any, 0, len(o.inv.Resources))
	for _, r := range o.inv.Resources {
		resources = append(resources, map[string]any{"file": r.QFile, "stack": r.Stack, "usedBy": r.UsedBy, "state": o.status[r.Unit]})
	}
	writeJSON(w, map[string]any{"stacks": o.views, "resources": resources, "warnings": o.warnings})
}

type overview struct {
	inv      *Inventory
	views    []stackView
	status   map[string]UnitState
	warnings []string
}

// overview scans the Quadlet directory and adds unit and container status
func (s *Server) overview(ctx context.Context) (*overview, error) {
	inv, err := Scan(s.cfg.QuadletDir)
	if err != nil {
		return nil, err
	}
	var units []string
	for _, st := range inv.Stacks {
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Missing systemd or Podman (e.g. while developing) degrades the view instead of failing it
	o := &overview{inv: inv}
	if len(inv.Linked) > 0 {
		o.warnings = append(o.warnings, "Quadstack does not manage symlinked files or folders, so these are not listed; their units still run: "+strings.Join(inv.Linked, ", "))
	}
	o.status, err = s.systemd.Status(ctx, units)
	if err != nil {
		o.warnings = append(o.warnings, err.Error())
	}
	containers, err := s.podman.Containers(ctx)
	if err != nil {
		o.warnings = append(o.warnings, err.Error())
	}
	byUnit := map[string][]Container{}
	for _, c := range containers {
		byUnit[c.Unit()] = append(byUnit[c.Unit()], c)
	}

	o.views = make([]stackView, 0, len(inv.Stacks))
	for _, st := range inv.Stacks {
		v := stackView{Stack: st, Units: map[string]UnitState{}, Containers: []Container{}}
		for _, f := range st.Files {
			if f.Unit != "" {
				v.Units[f.Unit] = o.status[f.Unit]
				v.Containers = append(v.Containers, byUnit[f.Unit]...)
			}
		}
		v.State = stackState(st.Units(), v.Units)
		o.views = append(o.views, v)
	}
	return o, nil
}

func stackState(units []string, status map[string]UnitState) string {
	if len(units) == 0 {
		return "empty"
	}
	active, failed, transitioning, unknown := 0, 0, false, false
	for _, u := range units {
		switch status[u].Active {
		case "active":
			active++
		case "activating", "deactivating", "reloading":
			transitioning = true
		case "failed":
			failed++
		case "inactive":
		default:
			unknown = true
		}
	}
	switch {
	case failed > 0:
		return "failed"
	case transitioning:
		return "busy"
	case unknown:
		return "unknown"
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

// unitStep is one systemd job of an action
type unitStep struct{ unit, action string }

// planUnits orders an action on units around shared network namespaces. A container that joins
// another's namespace (Network=container:…) loses it when that container stops or restarts, so
// it is stopped first and, on a restart, started again afterwards. parents maps such a unit to
// the unit it joins; active names the dependents outside units that run now.
func planUnits(units []string, action string, parents map[string]string, active func(string) bool) []unitStep {
	var deps, rest []string
	for d, p := range parents {
		if slices.Contains(units, p) && (slices.Contains(units, d) || active(d)) {
			deps = append(deps, d)
		}
	}
	slices.Sort(deps)
	for _, u := range units {
		if !slices.Contains(deps, u) {
			rest = append(rest, u)
		}
	}
	var steps []unitStep
	add := func(units []string, action string) {
		for _, u := range units {
			steps = append(steps, unitStep{u, action})
		}
	}
	switch action {
	case "start":
		// a namespace must exist before a container can join it
		for _, joins := range []bool{false, true} {
			for _, u := range units {
				if (parents[u] != "") == joins {
					steps = append(steps, unitStep{u, "start"})
				}
			}
		}
	case "stop":
		add(deps, "stop")
		add(rest, "stop")
	case "restart":
		add(deps, "stop")
		add(rest, "restart")
		add(deps, "start")
	}
	return steps
}

// unitPlan is planUnits for the scanned Quadlets and what systemd reports as running
func (s *Server) unitPlan(ctx context.Context, inv *Inventory, units []string, action string) ([]unitStep, error) {
	parents := inv.netnsParents()
	var outside []string
	for d, p := range parents {
		if action != "start" && slices.Contains(units, p) && !slices.Contains(units, d) {
			outside = append(outside, d)
		}
	}
	status := map[string]UnitState{}
	if len(outside) > 0 {
		var err error
		if status, err = s.systemd.Status(ctx, outside); err != nil {
			return nil, fmt.Errorf("checking the containers that share this network: %w", err)
		}
	}
	return planUnits(units, action, parents, func(u string) bool {
		a := status[u].Active
		return a == "active" || a == "activating" || a == "reloading"
	}), nil
}

// unitTasks runs the steps of a plan. A container is not started when the one whose network
// it shares failed to start or restart.
func (s *Server) unitTasks(inv *Inventory, steps []unitStep) []operationTask {
	parents := inv.netnsParents()
	failed := map[string]bool{}
	tasks := make([]operationTask, 0, len(steps))
	for _, step := range steps {
		tasks = append(tasks, simpleTask(step.action+" "+step.unit, step.unit, func(ctx context.Context) error {
			if p := parents[step.unit]; step.action == "start" && failed[p] {
				return fmt.Errorf("start %s: not attempted, because %s, whose network it shares, is not running", step.unit, p)
			}
			err := s.systemd.Action(ctx, step.unit, step.action)
			if err != nil && step.action != "stop" {
				failed[step.unit] = true
			}
			return err
		}))
	}
	return tasks
}

// POST /api/stack-action {stack, action}: start, stop or restart every unit of a stack
func (s *Server) stackAction(w http.ResponseWriter, r *http.Request) {
	var req struct{ Stack, Action string }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := checkAction(req.Action); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	inv, err := Scan(s.cfg.QuadletDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	st := inv.stack(req.Stack)
	if st == nil {
		writeErr(w, http.StatusNotFound, errors.New("unknown stack"))
		return
	}
	units := st.Units()
	if len(units) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("this stack has no runnable units"))
		return
	}
	steps, err := s.unitPlan(r.Context(), inv, units, req.Action)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.enqueue(w, req.Action+" stack "+st.Name, st.ID, s.unitTasks(inv, steps), true)
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
	if err := checkAction(req.Action); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	inv, err := Scan(s.cfg.QuadletDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	steps, err := s.unitPlan(r.Context(), inv, []string{req.Unit}, req.Action)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.enqueue(w, req.Action+" "+req.Unit, "", s.unitTasks(inv, steps), false)
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	s.enqueue(w, "Reload systemd", "", []operationTask{simpleTask("daemon-reload", "", s.systemd.Reload)}, false)
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
	if req.Apply != "" && req.Apply != "reload" && req.Apply != "restart" {
		writeErr(w, http.StatusBadRequest, errors.New("apply must be empty, reload or restart"))
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
		tasks := []operationTask{simpleTask("daemon-reload", "", s.systemd.Reload)}
		var err error
		if file := parseFile(s.files.abs(rel), rel); req.Apply == "restart" && file.Unit != "" {
			var restart []operationTask
			restart, err = s.restartTasks(r.Context(), file.Unit)
			tasks = append(tasks, restart...)
		}
		var operation Operation
		if err == nil {
			operation, err = s.operations.Submit(context.Background(), "Apply "+rel, "", tasks, false)
		}
		if err != nil {
			resp["applyError"] = "File saved, but applying was not queued: " + err.Error()
		} else {
			resp["operation"] = operation
		}
	}
	writeJSON(w, resp)
}

// restartTasks restarts a unit as the Quadlets are now, with the containers that share its network
func (s *Server) restartTasks(ctx context.Context, unit string) ([]operationTask, error) {
	inv, err := Scan(s.cfg.QuadletDir)
	if err != nil {
		return nil, err
	}
	steps, err := s.unitPlan(ctx, inv, []string{unit}, "restart")
	return s.unitTasks(inv, steps), err
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request)  { s.saveAndApply(w, r, false) }
func (s *Server) createFile(w http.ResponseWriter, r *http.Request) { s.saveAndApply(w, r, true) }

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	rel, ok := relParam(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	var tasks []operationTask
	if f := parseFile(s.files.abs(rel), rel); f.Unit != "" && r.URL.Query().Get("stop") == "1" {
		inv, err := Scan(s.cfg.QuadletDir)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		steps, err := s.unitPlan(r.Context(), inv, []string{f.Unit}, "stop")
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		tasks = s.unitTasks(inv, steps)
	}
	tasks = append(tasks, simpleTask("Delete "+rel, "", func(context.Context) error { return s.files.Delete(rel) }), simpleTask("daemon-reload after deletion", "", s.systemd.Reload))
	s.enqueue(w, "Delete "+rel, "", tasks, false)
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
	operation, err := s.operations.Submit(context.Background(), "Reload after moving "+from+" to "+to, "", []operationTask{simpleTask("daemon-reload", "", s.systemd.Reload)}, false)
	if err != nil {
		writeJSON(w, map[string]any{"ok": true, "applyError": "File moved, but reload was not queued: " + err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "operation": operation})
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

// GET /api/history/deleted lists deleted files whose versions can still be restored
func (s *Server) deletedFiles(w http.ResponseWriter, r *http.Request) {
	ds, err := s.files.Deleted()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, ds)
}

// DELETE /api/history?path= forgets every saved version of a file
func (s *Server) forgetHistory(w http.ResponseWriter, r *http.Request) {
	rel, ok := relParam(w, r.URL.Query().Get("path"))
	if !ok {
		return
	}
	if err := s.files.Forget(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) images(w http.ResponseWriter, r *http.Request) {
	is, err := s.podman.Images(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, is)
}

func (s *Server) pruneImages(w http.ResponseWriter, r *http.Request) {
	var req struct{ All bool }
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.enqueue(w, "Prune images", "", []operationTask{{name: "Prune images", run: func(ctx context.Context, report func(string)) error {
		output, err := s.podman.PruneImages(ctx, req.All)
		report(output)
		return err
	}}}, false)
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
	ns, err := s.podman.Networks(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	vs, err := s.podman.Volumes(r.Context())
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
	out, err := s.secretList(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, out)
}

// secretList combines Podman's secrets with the files that reference them
func (s *Server) secretList(ctx context.Context) ([]map[string]any, error) {
	ss, err := s.podman.Secrets(ctx)
	if err != nil {
		return nil, err
	}
	uses, err := s.secretUses()
	if err != nil {
		return nil, err
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
	return out, nil
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
	if !secretNameRe.MatchString(req.Name) || len(value) == 0 || len(value) > maxSecretSize {
		writeErr(w, http.StatusBadRequest, errors.New("use a valid secret name and a value of 1 byte to 512 KiB"))
		return
	}
	s.enqueue(w, "Set secret "+req.Name, "", []operationTask{simpleTask("Set secret "+req.Name, "", func(ctx context.Context) error {
		return s.podman.SetSecret(ctx, req.Name, value, req.Replace)
	})}, false)
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
	if !secretNameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, errors.New("invalid secret name"))
		return
	}
	if uses, err := s.secretUses(); err == nil && len(uses[name]) > 0 && r.URL.Query().Get("force") != "1" {
		writeErr(w, http.StatusConflict, errors.New("this secret is still used by "+uses[name][0].Path))
		return
	}
	s.enqueue(w, "Remove secret "+name, "", []operationTask{simpleTask("Remove secret "+name, "", func(ctx context.Context) error {
		return s.podman.RemoveSecret(ctx, name)
	})}, false)
}

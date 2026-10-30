package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP server is built per request for the caller's grant (the handler is stateless),
// so it lists only the tools the grant allows and an edit or revoke applies at once.

// errDenied marks a call the grant does not allow; the audit log records it as "denied"
type errDenied struct{ msg string }

func (e errDenied) Error() string { return e.msg }

func deny(format string, args ...any) error {
	return errDenied{"not allowed by this grant: " + fmt.Sprintf(format, args...)}
}

// scopeFor names what a file belongs to: its stack's ID, "shared" for a top-level network or
// volume, or for a file that is gone, the folder's stack. "" (unknown) needs scope "*".
func scopeFor(inv *Inventory, rel string) string {
	for _, st := range inv.Stacks {
		for _, f := range st.Files {
			if f.Path == rel {
				return st.ID
			}
		}
	}
	for _, r := range inv.Resources {
		if r.Path == rel {
			return firstNonEmpty(r.Stack, scopeShared)
		}
	}
	if first, _, nested := strings.Cut(rel, "/"); nested && !strings.HasSuffix(first, ".d") {
		return "d/" + first
	}
	return ""
}

func scopeOfUnit(inv *Inventory, unit string) string {
	for _, st := range inv.Stacks {
		for _, f := range st.Files {
			if f.Unit == unit {
				return st.ID
			}
		}
	}
	for _, r := range inv.Resources {
		if r.Unit == unit {
			return firstNonEmpty(r.Stack, scopeShared)
		}
	}
	return ""
}

func describeScope(scope string) string {
	if scope == "" {
		return "a file outside any stack"
	}
	return "stack " + scope
}

func (s *Server) mcpServer(g Grant) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: mcpServerName(), Title: "Quadstack on " + hostname(), Version: version}, &mcp.ServerOptions{
		Instructions: "Quadstack manages rootless Podman Quadlets on " + hostname() + ". Access is limited to what the grant " +
			strconv.Quote(g.Label) + " allows, until " + g.Expires.Format(time.RFC3339) + ". Call access_info to see the rules. " +
			"Stacks are identified as d/<folder> or r/<unit name>. Every call is logged.",
	})
	t := &toolset{s: s, srv: srv, grant: g}
	t.register()
	return srv
}

type toolset struct {
	s     *Server
	srv   *mcp.Server
	grant Grant
}

var (
	yes = true
	no  = false
)

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &no}
}

func changes(title string, destructive bool) *mcp.ToolAnnotations {
	d := &no
	if destructive {
		d = &yes
	}
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: d, OpenWorldHint: &no}
}

// add registers a tool when the grant has any of perms (none: always). Each call re-reads the
// grant, so a revoked, expired or narrowed grant takes effect mid-session, and is audited.
func add[In any](t *toolset, perms []string, tool *mcp.Tool, h func(ctx context.Context, g Grant, in In) (any, error)) {
	offered := len(perms) == 0
	for _, p := range perms {
		offered = offered || t.grant.Has(p)
	}
	if !offered {
		return
	}
	mcp.AddTool(t.srv, tool, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		entry := AuditEntry{Time: time.Now(), Grant: t.grant.ID, Label: t.grant.Label, Tool: tool.Name, Args: auditArgs(in), Result: "ok"}
		g, ok := t.s.grants.Get(t.grant.ID)
		var out any
		var err error
		if !ok {
			err = errDenied{"this grant has ended or was revoked"}
		} else {
			out, err = h(ctx, g, in)
		}
		if err != nil {
			entry.Result, entry.Error = "error", err.Error()
			if errors.As(err, new(errDenied)) {
				entry.Result = "denied"
			}
		}
		t.s.audit.Write(entry)
		return nil, out, err
	})
}

// need fails unless the grant allows perm for scope
func need(g Grant, perm, scope string) error {
	if !g.Allows(perm, scope) {
		return deny("%s on %s", perm, describeScope(scope))
	}
	return nil
}

func managedPath(p string) (string, error) {
	rel, err := cleanRel(p)
	if err == nil && !isManaged(path.Base(rel)) {
		err = errors.New("not a Quadlet, drop-in (.conf) or env file")
	}
	return rel, err
}

// tail keeps the last n bytes of b, which is the part of a log or command output that matters
func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return "…(truncated)\n" + string(b[len(b)-n:])
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

type (
	noArgs   struct{}
	pathArgs struct {
		Path string `json:"path" jsonschema:"file path relative to the Quadlet directory, e.g. media/sonarr.container"`
	}
	writeArgs struct {
		Path    string `json:"path" jsonschema:"file path relative to the Quadlet directory; a new folder makes a new stack"`
		Content string `json:"content" jsonschema:"the complete new file content"`
		Apply   string `json:"apply,omitempty" jsonschema:"after saving: reload (default, daemon-reload), restart (also restart this file's unit) or none"`
	}
	validateArgs struct {
		Path    string `json:"path" jsonschema:"file path relative to the Quadlet directory"`
		Content string `json:"content" jsonschema:"the complete file content to check"`
	}
	deleteArgs struct {
		Path string `json:"path" jsonschema:"file path relative to the Quadlet directory"`
		Stop bool   `json:"stop,omitempty" jsonschema:"stop the file's unit first (needs units.stop)"`
	}
	historyArgs struct {
		Path    string `json:"path" jsonschema:"file path relative to the Quadlet directory"`
		Version string `json:"version,omitempty" jsonschema:"a version ID from the list; omit to list versions"`
	}
	unitArgs struct {
		Unit   string `json:"unit" jsonschema:"systemd unit, e.g. sonarr.service"`
		Action string `json:"action" jsonschema:"start, stop or restart"`
	}
	stackArgs struct {
		Stack  string `json:"stack" jsonschema:"stack ID, e.g. d/media or r/arr"`
		Action string `json:"action" jsonschema:"start, stop or restart"`
	}
	logArgs struct {
		Unit  string `json:"unit,omitempty" jsonschema:"one unit; give unit or stack"`
		Stack string `json:"stack,omitempty" jsonschema:"all units of a stack; give unit or stack"`
		Lines int    `json:"lines,omitempty" jsonschema:"number of lines, default 200, at most 2000"`
		Since string `json:"since,omitempty" jsonschema:"journalctl --since value, e.g. '1 hour ago' or '2026-10-30 08:00'"`
	}
	execArgs struct {
		Container string   `json:"container" jsonschema:"container name or ID"`
		Command   []string `json:"command" jsonschema:"program and arguments, run without a shell unless you pass sh -c"`
		Timeout   int      `json:"timeoutSeconds,omitempty" jsonschema:"default 60, at most 600"`
	}
	pullArgs struct {
		Stack string `json:"stack" jsonschema:"stack ID whose container images to pull"`
	}
	pruneArgs struct {
		All bool `json:"all,omitempty" jsonschema:"also remove images no container uses, not only dangling ones"`
	}
	setSecretArgs struct {
		Name    string `json:"name" jsonschema:"secret name"`
		Value   string `json:"value" jsonschema:"the secret value"`
		Replace bool   `json:"replace,omitempty" jsonschema:"replace the value of an existing secret"`
	}
	removeSecretArgs struct {
		Name  string `json:"name" jsonschema:"secret name"`
		Force bool   `json:"force,omitempty" jsonschema:"delete even though files still reference it"`
	}
)

func (t *toolset) register() {
	s := t.s

	add(t, nil, &mcp.Tool{Name: "access_info", Description: "What this grant allows, on which stacks, and until when.", Annotations: readOnly("Access info")},
		func(ctx context.Context, g Grant, _ noArgs) (any, error) {
			return map[string]any{"grant": g.Label, "expires": g.Expires, "rules": g.Rules,
				"scopes": "\"*\" is every stack and the shared networks and volumes, \"shared\" only the top-level network and volume files"}, nil
		})

	add(t, []string{"stacks.read"}, &mcp.Tool{Name: "list_stacks", Description: "Stacks this grant can see, with files, unit state and containers.", Annotations: readOnly("List stacks")},
		func(ctx context.Context, g Grant, _ noArgs) (any, error) {
			o, err := s.overview(ctx)
			if err != nil {
				return nil, err
			}
			stacks := []map[string]any{}
			for _, v := range o.views {
				if !g.Allows("stacks.read", v.ID) {
					continue
				}
				var files []map[string]any
				for _, f := range v.Files {
					files = append(files, map[string]any{"path": f.Path, "kind": f.Kind, "unit": f.Unit, "image": f.Image, "autostart": f.Autostart, "parseError": f.ParseError})
				}
				var cs []map[string]string
				for _, c := range v.Containers {
					cs = append(cs, map[string]string{"name": strings.Join(c.Names, ","), "image": c.Image, "state": c.State, "status": c.Status, "unit": c.Unit()})
				}
				stacks = append(stacks, map[string]any{"id": v.ID, "name": v.Name, "state": v.State, "files": files, "units": v.Units, "containers": cs})
			}
			out := map[string]any{"stacks": stacks, "warnings": o.warnings}
			if g.Allows("stacks.read", scopeShared) {
				var shared []map[string]any
				for _, r := range o.inv.Resources {
					if r.Stack == "" {
						shared = append(shared, map[string]any{"path": r.Path, "kind": r.Kind, "name": r.Name, "usedBy": r.UsedBy, "state": o.status[r.Unit]})
					}
				}
				out["shared"] = shared
			}
			return out, nil
		})

	add(t, []string{"files.read"}, &mcp.Tool{Name: "read_file", Description: "Read a Quadlet, drop-in or env file.", Annotations: readOnly("Read file")},
		func(ctx context.Context, g Grant, in pathArgs) (any, error) {
			rel, err := managedPath(in.Path)
			if err != nil {
				return nil, err
			}
			inv, err := Scan(s.cfg.QuadletDir)
			if err != nil {
				return nil, err
			}
			scope := scopeFor(inv, rel)
			if err := need(g, "files.read", scope); err != nil {
				return nil, err
			}
			if strings.HasSuffix(rel, ".env") {
				if err := need(g, "env.read", scope); err != nil {
					return nil, err
				}
			}
			content, err := s.files.Read(rel)
			if err != nil {
				return nil, err
			}
			return map[string]string{"path": rel, "stack": scope, "content": content}, nil
		})

	// checkWrite allows a change when the file's stack before and after it are both writable;
	// a top-level container that changes Pod= moves to another stack
	checkWrite := func(g Grant, rel string, content *string) (string, error) {
		inv, err := Scan(s.cfg.QuadletDir)
		if err != nil {
			return "", err
		}
		if _, err := s.files.Read(rel); err == nil {
			if err := need(g, "files.write", scopeFor(inv, rel)); err != nil {
				return "", err
			}
		}
		after, err := s.files.ScanWith(rel, content)
		if err != nil {
			return "", err
		}
		scope := scopeFor(after, rel)
		return scope, need(g, "files.write", scope)
	}

	add(t, []string{"files.write"}, &mcp.Tool{Name: "validate_file", Description: "Check file content with Podman's quadlet generator without saving it.", Annotations: readOnly("Validate file")},
		func(ctx context.Context, g Grant, in validateArgs) (any, error) {
			rel, err := managedPath(in.Path)
			if err != nil {
				return nil, err
			}
			if _, err := checkWrite(g, rel, &in.Content); err != nil {
				return nil, err
			}
			return s.files.Validate(ctx, rel, &in.Content), nil
		})

	add(t, []string{"files.write"}, &mcp.Tool{Name: "write_file", Description: "Create or replace a file. It is validated first and not saved when validation fails; the previous version stays in the history.", Annotations: changes("Write file", false)},
		func(ctx context.Context, g Grant, in writeArgs) (any, error) {
			rel, err := managedPath(in.Path)
			if err != nil {
				return nil, err
			}
			apply := firstNonEmpty(in.Apply, "reload")
			if apply != "reload" && apply != "restart" && apply != "none" {
				return nil, errors.New("apply must be reload, restart or none")
			}
			scope, err := checkWrite(g, rel, &in.Content)
			if err != nil {
				return nil, err
			}
			if apply == "restart" {
				if err := need(g, "units.restart", scope); err != nil {
					return nil, err
				}
			}
			v := s.files.Validate(ctx, rel, &in.Content)
			if !v.OK {
				return nil, fmt.Errorf("validation failed, nothing was saved:\n%s", v.Output)
			}
			if err := s.files.Write(rel, in.Content); err != nil {
				return nil, err
			}
			res := map[string]any{"saved": rel, "stack": scope, "validation": v}
			if apply != "none" {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
				defer cancel()
				if err := s.systemd.Reload(ctx); err != nil {
					res["applyError"] = err.Error()
				} else if f := parseFile(s.files.abs(rel), rel); apply == "restart" && f.Unit != "" {
					if err := s.systemd.Action(ctx, f.Unit, "restart"); err != nil {
						res["applyError"] = err.Error()
					}
				}
			}
			return res, nil
		})

	add(t, []string{"files.write"}, &mcp.Tool{Name: "daemon_reload", Description: "systemctl --user daemon-reload, which regenerates the units from the Quadlet files.", Annotations: changes("Reload systemd", false)},
		func(ctx context.Context, g Grant, _ noArgs) (any, error) {
			return map[string]bool{"ok": true}, s.systemd.Reload(ctx)
		})

	add(t, []string{"files.delete"}, &mcp.Tool{Name: "delete_file", Description: "Delete a file; it stays restorable from the history in the web UI.", Annotations: changes("Delete file", true)},
		func(ctx context.Context, g Grant, in deleteArgs) (any, error) {
			rel, err := managedPath(in.Path)
			if err != nil {
				return nil, err
			}
			inv, err := Scan(s.cfg.QuadletDir)
			if err != nil {
				return nil, err
			}
			scope := scopeFor(inv, rel)
			if err := need(g, "files.delete", scope); err != nil {
				return nil, err
			}
			f := parseFile(s.files.abs(rel), rel)
			if in.Stop && f.Unit != "" {
				if err := need(g, "units.stop", scope); err != nil {
					return nil, err
				}
				s.systemd.Action(ctx, f.Unit, "stop")
			}
			if err := s.files.Delete(rel); err != nil {
				return nil, err
			}
			s.systemd.Reload(ctx)
			return map[string]string{"deleted": rel}, nil
		})

	add(t, []string{"history.read"}, &mcp.Tool{Name: "file_history", Description: "List the saved versions of a file, or read one version.", Annotations: readOnly("File history")},
		func(ctx context.Context, g Grant, in historyArgs) (any, error) {
			rel, err := managedPath(in.Path)
			if err != nil {
				return nil, err
			}
			inv, err := Scan(s.cfg.QuadletDir)
			if err != nil {
				return nil, err
			}
			scope := scopeFor(inv, rel)
			if err := need(g, "history.read", scope); err != nil {
				return nil, err
			}
			if strings.HasSuffix(rel, ".env") {
				if err := need(g, "env.read", scope); err != nil {
					return nil, err
				}
			}
			if in.Version == "" {
				return s.files.History(rel)
			}
			content, err := s.files.ReadVersion(rel, in.Version)
			return map[string]string{"path": rel, "version": in.Version, "content": content}, err
		})

	unitPerms := []string{"units.start", "units.stop", "units.restart"}
	checkAction := func(action string) error {
		if action != "start" && action != "stop" && action != "restart" {
			return errors.New("action must be start, stop or restart")
		}
		return nil
	}

	add(t, unitPerms, &mcp.Tool{Name: "unit_action", Description: "Start, stop or restart one unit.", Annotations: changes("Unit action", true)},
		func(ctx context.Context, g Grant, in unitArgs) (any, error) {
			if err := checkAction(in.Action); err != nil {
				return nil, err
			}
			if !unitRe.MatchString(in.Unit) {
				return nil, errors.New("invalid unit")
			}
			inv, err := Scan(s.cfg.QuadletDir)
			if err != nil {
				return nil, err
			}
			if err := need(g, "units."+in.Action, scopeOfUnit(inv, in.Unit)); err != nil {
				return nil, err
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			return map[string]bool{"ok": true}, s.systemd.Action(ctx, in.Unit, in.Action)
		})

	add(t, unitPerms, &mcp.Tool{Name: "stack_action", Description: "Start, stop or restart every unit of a stack.", Annotations: changes("Stack action", true)},
		func(ctx context.Context, g Grant, in stackArgs) (any, error) {
			if err := checkAction(in.Action); err != nil {
				return nil, err
			}
			if err := need(g, "units."+in.Action, in.Stack); err != nil {
				return nil, err
			}
			st, err := FindStack(s.cfg.QuadletDir, in.Stack)
			if err != nil {
				return nil, errors.New("unknown stack")
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
			var errs []error
			for _, u := range st.Units() {
				errs = append(errs, s.systemd.Action(ctx, u, in.Action))
			}
			return map[string]any{"ok": true, "units": st.Units()}, errors.Join(errs...)
		})

	add(t, []string{"logs.read"}, &mcp.Tool{Name: "read_logs", Description: "Recent journal lines of a unit or of all units of a stack.", Annotations: readOnly("Read logs")},
		func(ctx context.Context, g Grant, in logArgs) (any, error) {
			inv, err := Scan(s.cfg.QuadletDir)
			if err != nil {
				return nil, err
			}
			var units []string
			switch {
			case in.Unit != "" && in.Stack == "":
				if !unitRe.MatchString(in.Unit) {
					return nil, errors.New("invalid unit")
				}
				if err := need(g, "logs.read", scopeOfUnit(inv, in.Unit)); err != nil {
					return nil, err
				}
				units = []string{in.Unit}
			case in.Stack != "" && in.Unit == "":
				if err := need(g, "logs.read", in.Stack); err != nil {
					return nil, err
				}
				for _, st := range inv.Stacks {
					if st.ID == in.Stack {
						for _, f := range st.Files {
							if f.Unit != "" {
								units = append(units, f.Unit)
							}
						}
					}
				}
				if len(units) == 0 {
					return nil, errors.New("this stack has no units")
				}
			default:
				return nil, errors.New("give either unit or stack")
			}
			lines := in.Lines
			if lines <= 0 {
				lines = 200
			}
			args := []string{"--user", "--no-pager", "--output=short-iso", "--lines=" + strconv.Itoa(min(lines, 2000))}
			if in.Since != "" {
				args = append(args, "--since="+in.Since)
			}
			for _, u := range units {
				args = append(args, "--unit="+u)
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, "journalctl", args...).CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("journalctl: %s", firstNonEmpty(strings.TrimSpace(string(out)), err.Error()))
			}
			if bytes.Contains(out, []byte("insufficient permissions")) {
				return nil, errors.New(journalProblem(ctx))
			}
			return map[string]any{"units": units, "log": tail(out, 256<<10)}, nil
		})

	if s.cfg.Terminal {
		add(t, []string{"exec"}, &mcp.Tool{Name: "exec", Description: "Run a command inside a running container and return its output. No terminal; stdin is empty.", Annotations: changes("Exec in container", true)},
			func(ctx context.Context, g Grant, in execArgs) (any, error) {
				if len(in.Command) == 0 {
					return nil, errors.New("give a command")
				}
				cs, err := Containers(ctx)
				if err != nil {
					return nil, err
				}
				var found *Container
				for i, c := range cs {
					if (len(in.Container) >= 12 && strings.HasPrefix(c.ID, in.Container)) || slices.Contains(c.Names, in.Container) {
						found = &cs[i]
					}
				}
				if found == nil {
					return nil, errors.New("no such container")
				}
				inv, err := Scan(s.cfg.QuadletDir)
				if err != nil {
					return nil, err
				}
				if err := need(g, "exec", scopeOfUnit(inv, found.Unit())); err != nil {
					return nil, err
				}
				timeout := in.Timeout
				if timeout <= 0 {
					timeout = 60
				}
				ctx, cancel := context.WithTimeout(ctx, time.Duration(min(timeout, 600))*time.Second)
				defer cancel()
				var stdout, stderr bytes.Buffer
				cmd := exec.CommandContext(ctx, "podman", append([]string{"exec", found.ID}, in.Command...)...)
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				code := 0
				if err := cmd.Run(); err != nil {
					var ee *exec.ExitError
					if !errors.As(err, &ee) {
						return nil, err
					}
					code = ee.ExitCode()
				}
				return map[string]any{"exitCode": code, "stdout": tail(stdout.Bytes(), 64<<10), "stderr": tail(stderr.Bytes(), 16<<10)}, nil
			})
	}

	add(t, []string{"images.pull"}, &mcp.Tool{Name: "pull_images", Description: "Pull newer versions of a stack's container images. Restart the stack afterwards to use them.", Annotations: changes("Pull images", false)},
		func(ctx context.Context, g Grant, in pullArgs) (any, error) {
			if err := need(g, "images.pull", in.Stack); err != nil {
				return nil, err
			}
			st, err := FindStack(s.cfg.QuadletDir, in.Stack)
			if err != nil {
				return nil, errors.New("unknown stack")
			}
			var res []map[string]string
			for _, f := range st.Files {
				if f.Image == "" || strings.HasSuffix(f.Image, ".image") || strings.HasSuffix(f.Image, ".build") {
					continue
				}
				pctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
				out, err := exec.CommandContext(pctx, "podman", "pull", "--quiet", f.Image).CombinedOutput()
				cancel()
				r := map[string]string{"image": f.Image, "id": strings.TrimSpace(string(out))}
				if err != nil {
					r = map[string]string{"image": f.Image, "error": firstNonEmpty(strings.TrimSpace(string(out)), err.Error())}
				}
				res = append(res, r)
			}
			return res, nil
		})

	add(t, []string{"images.read"}, &mcp.Tool{Name: "list_images", Description: "Images in local storage.", Annotations: readOnly("List images")},
		func(ctx context.Context, g Grant, _ noArgs) (any, error) { return Images(ctx) })

	add(t, []string{"images.prune"}, &mcp.Tool{Name: "prune_images", Description: "Remove dangling images, or with all, every image no container uses.", Annotations: changes("Prune images", true)},
		func(ctx context.Context, g Grant, in pruneArgs) (any, error) {
			out, err := PruneImages(ctx, in.All)
			return map[string]string{"output": out}, err
		})

	add(t, []string{"resources.read"}, &mcp.Tool{Name: "list_resources", Description: "Podman networks and volumes, and the Quadlet network and volume files with the stacks that use them.", Annotations: readOnly("List networks and volumes")},
		func(ctx context.Context, g Grant, _ noArgs) (any, error) {
			ns, err := Networks(ctx)
			if err != nil {
				return nil, err
			}
			vs, err := Volumes(ctx)
			if err != nil {
				return nil, err
			}
			inv, err := Scan(s.cfg.QuadletDir)
			if err != nil {
				return nil, err
			}
			var files []map[string]any
			for _, r := range inv.Resources {
				files = append(files, map[string]any{"path": r.Path, "kind": r.Kind, "name": r.Name, "stack": r.Stack, "usedBy": r.UsedBy})
			}
			return map[string]any{"networks": ns, "volumes": vs, "quadletFiles": files}, nil
		})

	add(t, []string{"secrets.read"}, &mcp.Tool{Name: "list_secrets", Description: "Secret names, dates and the files that use them. Values are never returned.", Annotations: readOnly("List secrets")},
		func(ctx context.Context, g Grant, _ noArgs) (any, error) { return s.secretList(ctx) })

	add(t, []string{"secrets.write"}, &mcp.Tool{Name: "set_secret", Description: "Create a secret, or replace its value. Running containers keep the old value until they restart.", Annotations: changes("Set secret", false)},
		func(ctx context.Context, g Grant, in setSecretArgs) (any, error) {
			return map[string]bool{"ok": true}, SetSecret(ctx, in.Name, []byte(in.Value), in.Replace)
		})

	add(t, []string{"secrets.delete"}, &mcp.Tool{Name: "remove_secret", Description: "Delete a secret. Refused while files reference it, unless force is set.", Annotations: changes("Delete secret", true)},
		func(ctx context.Context, g Grant, in removeSecretArgs) (any, error) {
			if uses, err := s.secretUses(); err == nil && len(uses[in.Name]) > 0 && !in.Force {
				return nil, errors.New("this secret is still used by " + uses[in.Name][0].Path)
			}
			return map[string]bool{"ok": true}, RemoveSecret(ctx, in.Name)
		})
}

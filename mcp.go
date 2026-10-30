package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// AI access over MCP is off until someone signed in creates a grant. A grant has a bearer
// token, an expiry and rules: which permission applies to which stacks. Ending a grant keeps
// its token, so reactivating it lets the same client back in; deleting it drops the token.
// Nothing that is not granted is offered as a tool, and every call is checked again and
// written to an audit log with the caller's address.

// Perm is one thing a grant can allow. Scoped permissions name the stacks they apply to;
// the others apply to the whole account.
type Perm struct {
	ID     string `json:"id"`
	Group  string `json:"group"`
	Label  string `json:"label"`
	Help   string `json:"help"`
	Scoped bool   `json:"scoped"`
}

var mcpPerms = []Perm{
	{"stacks.read", "Stacks", "See", "List the stack, its files, units and container status", true},
	{"files.read", "Stacks", "Read", "Read Quadlet and drop-in files", true},
	{"env.read", "Stacks", "Env", "Read .env files, which often hold credentials", true},
	{"history.read", "Stacks", "History", "Read earlier versions of files", true},
	{"files.write", "Stacks", "Write", "Create and change Quadlet source files directly in the stack directory and daemon-reload", true},
	{"files.delete", "Stacks", "Delete", "Delete files", true},
	{"logs.read", "Stacks", "Logs", "Read the journal of the stack's units", true},
	{"units.start", "Stacks", "Start", "Start units", true},
	{"units.stop", "Stacks", "Stop", "Stop units", true},
	{"units.restart", "Stacks", "Restart", "Restart units, also after saving a file", true},
	{"images.pull", "Stacks", "Pull", "Pull newer images of the stack's containers", true},
	{"exec", "Stacks", "Exec", "Run commands inside the stack's containers", true},

	{"resources.read", "Account", "Networks & volumes", "List Podman networks and volumes and which stacks use them", false},
	{"images.read", "Account", "List images", "List images and their sizes", false},
	{"images.prune", "Account", "Prune images", "Remove unused images", false},
	{"secrets.read", "Account", "List secrets", "Secret names and where they are used; values are never readable", false},
	{"secrets.write", "Account", "Set secrets", "Create secrets and replace their values", false},
	{"secrets.delete", "Account", "Delete secrets", "Delete secrets", false},
}

// scopeAll covers every stack, present and future, and the shared networks and volumes;
// scopeShared covers only the top-level network and volume files.
const (
	scopeAll    = "*"
	scopeShared = "shared"
)

var stackIDRe = regexp.MustCompile(`^[dr]/[^/]+$`)

func findPerm(id string) (Perm, bool) {
	for _, p := range mcpPerms {
		if p.ID == id {
			return p, true
		}
	}
	return Perm{}, false
}

type Grant struct {
	ID        string              `json:"id"`
	Label     string              `json:"label"`
	CreatedBy string              `json:"createdBy"`
	Created   time.Time           `json:"created"`
	Expires   time.Time           `json:"expires"`
	Revoked   *time.Time          `json:"revoked,omitempty"` // ended by hand; reactivating clears it
	LastUsed  *time.Time          `json:"lastUsed,omitempty"`
	LastFrom  string              `json:"lastFrom,omitempty"` // the address of the last call
	Rules     map[string][]string `json:"rules"`              // permission -> scopes; account-wide permissions use ["*"]
}

func (g *Grant) Active() bool {
	return g.Revoked == nil && time.Now().Before(g.Expires)
}

// Allows reports whether the grant allows perm for a scope (a stack ID or "shared")
func (g *Grant) Allows(perm, scope string) bool {
	for _, s := range g.Rules[perm] {
		if s == scopeAll || s == scope {
			return true
		}
	}
	return false
}

// Has reports whether perm is granted for anything at all
func (g *Grant) Has(perm string) bool {
	return len(g.Rules[perm]) > 0
}

func cleanRules(in map[string][]string) (map[string][]string, error) {
	out := map[string][]string{}
	for id, scopes := range in {
		p, ok := findPerm(id)
		if !ok {
			return nil, fmt.Errorf("unknown permission %q", id)
		}
		var keep []string
		for _, s := range scopes {
			switch {
			case s == scopeAll:
			case !p.Scoped:
				return nil, fmt.Errorf("%s applies to the whole account, so its scope must be %q", id, scopeAll)
			case s != scopeShared && !stackIDRe.MatchString(s):
				return nil, fmt.Errorf("invalid scope %q for %s", s, id)
			}
			if !slices.Contains(keep, s) {
				keep = append(keep, s)
			}
		}
		if slices.Contains(keep, scopeAll) {
			keep = []string{scopeAll}
		}
		if len(keep) > 0 {
			sort.Strings(keep)
			out[id] = keep
		}
	}
	return out, nil
}

type storedGrant struct {
	Grant
	TokenHash string `json:"tokenHash"`
}

// Grants keeps the grants in StateDir/mcp-grants.json
type Grants struct {
	mu       sync.Mutex
	path     string
	list     []*storedGrant
	loaded   bool
	lastSave time.Time
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func (gs *Grants) load() error {
	if gs.loaded {
		return nil
	}
	b, err := os.ReadFile(gs.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &gs.list); err != nil {
			return fmt.Errorf("reading %s: %w", gs.path, err)
		}
	}
	gs.loaded = true
	return nil
}

func (gs *Grants) save() error {
	b, err := json.MarshalIndent(gs.list, "", "  ")
	if err != nil {
		return err
	}
	tmp := gs.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	gs.lastSave = time.Now()
	return os.Rename(tmp, gs.path)
}

func (gs *Grants) List() ([]Grant, error) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if err := gs.load(); err != nil {
		return nil, err
	}
	out := []Grant{}
	for _, g := range gs.list {
		out = append(out, g.Grant)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Create returns the new grant and its token, which is shown once and stored only as a hash
func (gs *Grants) Create(by, label string, ttl time.Duration, rules map[string][]string) (Grant, string, error) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if err := gs.load(); err != nil {
		return Grant{}, "", err
	}
	token := "qsm_" + randomHex(32)
	now := time.Now()
	g := &storedGrant{
		Grant:     Grant{ID: randomHex(8), Label: label, CreatedBy: by, Created: now, Expires: now.Add(ttl), Rules: rules},
		TokenHash: hashToken(token),
	}
	gs.list = append(gs.list, g)
	return g.Grant, token, gs.save()
}

// Update changes a grant's label and rules, and its expiry when ttl > 0. An ended grant
// needs a ttl, and becomes active again with its old token.
func (gs *Grants) Update(id, label string, ttl time.Duration, rules map[string][]string) (Grant, error) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if err := gs.load(); err != nil {
		return Grant{}, err
	}
	for _, g := range gs.list {
		if g.ID != id {
			continue
		}
		if !g.Active() && ttl <= 0 {
			return Grant{}, errors.New("this grant has ended; choose how long it should run again")
		}
		g.Label, g.Rules = label, rules
		if ttl > 0 {
			g.Expires, g.Revoked = time.Now().Add(ttl), nil
		}
		return g.Grant, gs.save()
	}
	return Grant{}, os.ErrNotExist
}

// End stops a grant at once; its token keeps working after a reactivation
func (gs *Grants) End(id string) error {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if err := gs.load(); err != nil {
		return err
	}
	for _, g := range gs.list {
		if g.ID == id {
			if g.Revoked == nil {
				now := time.Now()
				g.Revoked = &now
			}
			return gs.save()
		}
	}
	return os.ErrNotExist
}

// Delete removes a grant and with it its token, for good
func (gs *Grants) Delete(id string) error {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if err := gs.load(); err != nil {
		return err
	}
	n := len(gs.list)
	gs.list = slices.DeleteFunc(gs.list, func(g *storedGrant) bool { return g.ID == id })
	if len(gs.list) == n {
		return os.ErrNotExist
	}
	return gs.save()
}

// ByToken returns the active grant a bearer token belongs to and records the call's address
func (gs *Grants) ByToken(token, from string) (Grant, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if token == "" || gs.load() != nil {
		return Grant{}, false
	}
	h := hashToken(token)
	for _, g := range gs.list {
		if subtle.ConstantTimeCompare([]byte(h), []byte(g.TokenHash)) == 1 && g.Active() {
			now := time.Now()
			changed := g.LastFrom != from
			g.LastUsed, g.LastFrom = &now, from
			// A new address is saved at once, so it shows even if the service stops right after
			if changed || now.Sub(gs.lastSave) > time.Minute {
				gs.save()
			}
			return g.Grant, true
		}
	}
	return Grant{}, false
}

// Get returns a grant by ID while it is active, so a call made after a revoke or edit sees it
func (gs *Grants) Get(id string) (Grant, bool) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.load() != nil {
		return Grant{}, false
	}
	for _, g := range gs.list {
		if g.ID == id && g.Active() {
			return g.Grant, true
		}
	}
	return Grant{}, false
}

/* Audit log: one JSON object per line in StateDir/mcp-audit.jsonl */

type AuditEntry struct {
	Time   time.Time       `json:"time"`
	Grant  string          `json:"grant"`
	Label  string          `json:"label"`
	From   string          `json:"from,omitempty"` // the caller's address
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result string          `json:"result"` // ok, denied or error
	Error  string          `json:"error,omitempty"`
}

const auditMaxSize = 5 << 20

type Audit struct {
	mu   sync.Mutex
	path string
}

func (a *Audit) Write(e AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st, err := os.Stat(a.path); err == nil && st.Size() > auditMaxSize {
		os.Rename(a.path, a.path+".1")
	}
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	f.Write(append(b, '\n'))
}

// Recent returns up to n entries, newest first
func (a *Audit) Recent(n int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []AuditEntry{}
	f, err := os.Open(a.path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var e AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	slices.Reverse(out)
	return out[:min(n, len(out))]
}

// auditArgs records what a call was about without file contents or secret values
func auditArgs(in any) json.RawMessage {
	b, _ := json.Marshal(in)
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return b
	}
	if c, ok := m["content"].(string); ok {
		m["content"] = fmt.Sprintf("<%d bytes, sha256 %s>", len(c), hashToken(c)[:12])
	}
	if _, ok := m["value"]; ok {
		m["value"] = "<redacted>"
	}
	b, _ = json.Marshal(m)
	return b
}

/* HTTP: the management API for signed-in users, and the bearer-authenticated MCP endpoint */

type grantCtxKey struct{}

type mcpCaller struct {
	grant Grant
	from  string
}

func callerFrom(ctx context.Context) (mcpCaller, bool) {
	c, ok := ctx.Value(grantCtxKey{}).(mcpCaller)
	return c, ok
}

// clientAddr is the address a request came from. Behind a reverse proxy the client's own
// address is in X-Forwarded-For, which anyone can set, so the proxy is named too.
func clientAddr(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		client, _, _ := strings.Cut(fwd, ",")
		return strings.TrimSpace(client) + " via " + peer
	}
	return peer
}

// mcpEndpoint authenticates the bearer token and hands the request to the MCP handler
func (s *Server) mcpEndpoint(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.MCPMaxTTL <= 0 {
			writeErr(w, http.StatusNotFound, errors.New("MCP access is disabled (QUADSTACK_MCP_MAX_HOURS=0)"))
			return
		}
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		from := clientAddr(r)
		g, ok := s.grants.ByToken(strings.TrimSpace(token), from)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="quadstack"`)
			writeErr(w, http.StatusUnauthorized, errors.New("no active MCP grant for this token"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantCtxKey{}, mcpCaller{g, from})))
	})
}

// GET /api/mcp: the settings page
func (s *Server) mcpInfo(w http.ResponseWriter, r *http.Request) {
	gs, err := s.grants.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{
		"enabled":     s.cfg.MCPMaxTTL > 0,
		"maxMinutes":  int(s.cfg.MCPMaxTTL / time.Minute),
		"endpoint":    s.cfg.PublicURL + "/mcp",
		"serverName":  mcpServerName(),
		"permissions": mcpPerms,
		"grants":      gs,
		"audit":       s.audit.Recent(200),
	})
}

type grantReq struct {
	Label   string              `json:"label"`
	Minutes int                 `json:"minutes"`
	Rules   map[string][]string `json:"rules"`
}

func (s *Server) readGrantReq(w http.ResponseWriter, r *http.Request, update bool) (grantReq, map[string][]string, bool) {
	var req grantReq
	err := readJSON(r, &req)
	req.Label = strings.TrimSpace(req.Label)
	ttl := time.Duration(req.Minutes) * time.Minute
	var rules map[string][]string
	switch {
	case err != nil:
	case s.cfg.MCPMaxTTL <= 0:
		err = errors.New("MCP access is disabled (QUADSTACK_MCP_MAX_HOURS=0)")
	case req.Label == "" || len(req.Label) > 80:
		err = errors.New("give the grant a name of up to 80 characters")
	case (!update || req.Minutes != 0) && (ttl < time.Minute || ttl > s.cfg.MCPMaxTTL):
		err = fmt.Errorf("the duration must be between 1 minute and %s", s.cfg.MCPMaxTTL)
	default:
		rules, err = cleanRules(req.Rules)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return req, nil, false
	}
	return req, rules, true
}

// POST /api/mcp/grants {label, minutes, rules}
func (s *Server) createGrant(w http.ResponseWriter, r *http.Request) {
	req, rules, ok := s.readGrantReq(w, r, false)
	if !ok {
		return
	}
	sess, _ := s.auth.Session(r)
	g, token, err := s.grants.Create(firstNonEmpty(sess.Name, sess.Sub), req.Label, time.Duration(req.Minutes)*time.Minute, rules)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{"grant": g, "token": token})
}

// PUT /api/mcp/grants/{id} {label, minutes (0 keeps the expiry), rules}
func (s *Server) updateGrant(w http.ResponseWriter, r *http.Request) {
	req, rules, ok := s.readGrantReq(w, r, true)
	if !ok {
		return
	}
	g, err := s.grants.Update(r.PathValue("id"), req.Label, time.Duration(req.Minutes)*time.Minute, rules)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, g)
}

// POST /api/mcp/grants/{id}/end ends a grant at once and keeps its token
func (s *Server) endGrant(w http.ResponseWriter, r *http.Request) {
	if err := s.grants.End(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// DELETE /api/mcp/grants/{id} removes a grant and its token
func (s *Server) deleteGrant(w http.ResponseWriter, r *http.Request) {
	if err := s.grants.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// mcpServerName names this machine's MCP server, quadstack-<host>, so a client that adds
// several machines keeps them apart (Claude Code prefixes tools with it)
func mcpServerName() string {
	h := strings.ToLower(hostname())
	h, _, _ = strings.Cut(h, ".")
	h = strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(h, "-"), "-")
	if h == "" {
		return "quadstack"
	}
	return "quadstack-" + h
}

func newGrants(cfg *Config) *Grants {
	return &Grants{path: filepath.Join(cfg.StateDir, "mcp-grants.json")}
}

func newAudit(cfg *Config) *Audit {
	return &Audit{path: filepath.Join(cfg.StateDir, "mcp-audit.jsonl")}
}

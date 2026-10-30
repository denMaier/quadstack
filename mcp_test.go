package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func mcpSession(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	return c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
}

func callText(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestMCP(t *testing.T) {
	s, c := testServer(t)
	h := s.Routes()
	ts := httptest.NewServer(h)
	defer ts.Close()
	for rel, content := range map[string]string{
		"web/web.container": "[Container]\nImage=nginx\n",
		"web/web.env":       "PASSWORD=hunter2\n",
		"db/db.container":   "[Container]\nImage=postgres\n",
		"arr.pod":           "[Pod]\n",
		"app.container":     "[Container]\nImage=app\n",
	} {
		if err := s.files.Write(rel, content); err != nil {
			t.Fatal(err)
		}
	}

	// Off until a grant exists
	if w := do(t, h, nil, "POST", "/mcp", `{}`, false); w.Code != 401 {
		t.Fatalf("no grant: %d", w.Code)
	}
	if n := mcpServerName(); !regexp.MustCompile(`^quadstack(-[a-z0-9]+(-[a-z0-9]+)*)?$`).MatchString(n) {
		t.Fatalf("server name %q", n)
	}
	if _, err := mcpSession(t, ts.URL, "qsm_wrong"); err == nil {
		t.Fatal("a wrong token must not connect")
	}

	grant := func(method, url, body string) map[string]any {
		t.Helper()
		w := do(t, h, c, method, url, body, true)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, url, w.Code, w.Body)
		}
		var m map[string]any
		json.Unmarshal(w.Body.Bytes(), &m)
		return m
	}
	if w := do(t, h, c, "POST", "/api/mcp/grants", `{"label":"x","minutes":600,"rules":{}}`, true); w.Code != 400 {
		t.Fatalf("longer than the maximum: %d", w.Code)
	}
	if w := do(t, h, c, "POST", "/api/mcp/grants", `{"label":"x","minutes":5,"rules":{"images.read":["d/web"]}}`, true); w.Code != 400 {
		t.Fatalf("account-wide permission with a stack scope: %d", w.Code)
	}
	created := grant("POST", "/api/mcp/grants", `{"label":"claude","minutes":30,"rules":{"stacks.read":["d/web"],"files.read":["d/web"]}}`)
	token := created["token"].(string)
	id := created["grant"].(map[string]any)["id"].(string)
	if raw, _ := os.ReadFile(s.grants.path); strings.Contains(string(raw), token) {
		t.Fatal("the token must only be stored as a hash")
	}

	cs, err := mcpSession(t, ts.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"access_info", "list_stacks", "read_file"}) {
		t.Fatalf("tools = %v", names)
	}

	if out, isErr := callText(t, cs, "list_stacks", nil); isErr || !strings.Contains(out, `"d/web"`) || strings.Contains(out, `"d/db"`) {
		t.Fatalf("list_stacks shows only granted stacks: %s", out)
	}
	if out, isErr := callText(t, cs, "read_file", map[string]any{"path": "web/web.container"}); isErr || !strings.Contains(out, "nginx") {
		t.Fatalf("read granted: %s", out)
	}
	if out, isErr := callText(t, cs, "read_file", map[string]any{"path": "db/db.container"}); !isErr || !strings.Contains(out, "not allowed") {
		t.Fatalf("read other stack: %s", out)
	}
	if out, isErr := callText(t, cs, "read_file", map[string]any{"path": "web/web.env"}); !isErr || strings.Contains(out, "hunter2") {
		t.Fatalf("env files need env.read: %s", out)
	}

	// Edits apply to the open session
	grant("PUT", "/api/mcp/grants/"+id, `{"label":"claude","rules":{"files.write":["d/web","r/app","d/not-created"]}}`)
	if out, isErr := callText(t, cs, "write_file", map[string]any{"path": "web/web.container", "content": "[Container]\nImage=nginx:2\n", "apply": "none"}); isErr {
		t.Fatalf("write granted: %s", out)
	}
	if b, _ := s.files.Read("web/web.container"); !strings.Contains(b, "nginx:2") {
		t.Fatal("not written")
	}
	// Joining the pod moves app.container into stack r/arr, which is not writable
	if out, isErr := callText(t, cs, "write_file", map[string]any{"path": "app.container", "content": "[Container]\nImage=app\nPod=arr.pod\n", "apply": "none"}); !isErr || !strings.Contains(out, "r/arr") {
		t.Fatalf("moving into another stack: %s", out)
	}
	if out, isErr := callText(t, cs, "write_file", map[string]any{"path": "web/web.container", "content": "x", "apply": "restart"}); !isErr || !strings.Contains(out, "units.restart") {
		t.Fatalf("restart needs units.restart: %s", out)
	}
	for _, p := range []string{"web/web.env", "web/web.container.d/override.conf", "web/nested/evil.container"} {
		if out, isErr := callText(t, cs, "write_file", map[string]any{"path": p, "content": "x", "apply": "none"}); !isErr {
			t.Fatalf("write %s should be rejected: %s", p, out)
		}
	}
	for _, p := range []string{"db/db.container", "db/new.container", "arr.pod", "other.container", "not-created/new.container"} {
		if out, isErr := callText(t, cs, "write_file", map[string]any{"path": p, "content": "[Container]\nImage=nginx\n", "apply": "none"}); !isErr {
			t.Fatalf("write outside an existing granted stack %s: %s", p, out)
		}
	}
	if out, isErr := callText(t, cs, "write_file", map[string]any{"path": "web/new.container", "content": "[Container]\nImage=nginx\n", "apply": "none"}); isErr {
		t.Fatalf("new Quadlet in granted stack: %s", out)
	}
	// files.read was removed by the edit, so the tool is no longer offered at all
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{"path": "web/web.container"}}); err == nil {
		t.Fatal("read_file should be gone")
	}

	// Ending keeps the token: the same client gets back in once the grant is reactivated
	grant("POST", "/api/mcp/grants/"+id+"/end", "")
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "access_info"}); err == nil {
		t.Fatal("an ended grant must not answer")
	}
	if w := do(t, h, c, "PUT", "/api/mcp/grants/"+id, `{"label":"claude","rules":{}}`, true); w.Code != 400 {
		t.Fatalf("reactivating needs a duration: %d", w.Code)
	}
	grant("PUT", "/api/mcp/grants/"+id, `{"label":"claude","minutes":10,"rules":{"stacks.read":["d/web"]}}`)
	// The client dropped its session on the 401 and reconnects, as Claude Code does, with the same token
	cs, err = mcpSession(t, ts.URL, token)
	if err != nil {
		t.Fatalf("same token after reactivating: %v", err)
	}
	defer cs.Close()
	if out, isErr := callText(t, cs, "access_info", nil); isErr {
		t.Fatalf("reactivated grant: %s", out)
	}
	var info struct{ Grants []Grant }
	json.Unmarshal(do(t, h, c, "GET", "/api/mcp", "", false).Body.Bytes(), &info)
	if len(info.Grants) != 1 || info.Grants[0].LastFrom != "127.0.0.1" {
		t.Fatalf("last caller address: %+v", info.Grants)
	}

	// Deleting drops the token for good
	grant("DELETE", "/api/mcp/grants/"+id, "")
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "access_info"}); err == nil {
		t.Fatal("a deleted grant must not answer")
	}
	if w := do(t, h, c, "PUT", "/api/mcp/grants/"+id, `{"label":"claude","minutes":10,"rules":{}}`, true); w.Code != 400 {
		t.Fatalf("a deleted grant cannot be reactivated: %d", w.Code)
	}

	audit := s.audit.Recent(50)
	var results []string
	for _, e := range audit {
		results = append(results, e.Tool+":"+e.Result)
		if e.From != "127.0.0.1" {
			t.Fatalf("audit entry without the caller's address: %+v", e)
		}
		if strings.Contains(string(e.Args), "nginx:2") {
			t.Fatalf("file contents must not be audited: %s", e.Args)
		}
	}
	if !slices.Contains(results, "read_file:denied") || !slices.Contains(results, "write_file:ok") {
		t.Fatalf("audit = %v", results)
	}
}

func TestMCPRejectsSymlinks(t *testing.T) {
	s, _ := testServer(t)
	if err := s.files.Write("web/app.container", "original"); err != nil {
		t.Fatal(err)
	}
	if err := s.files.Write("db/secret.container", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(s.cfg.QuadletDir, "db"), filepath.Join(s.cfg.QuadletDir, "web", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(s.cfg.QuadletDir, "db", "secret.container"), filepath.Join(s.cfg.QuadletDir, "web", "linked.container")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"web/link/secret.container", "web/linked.container"} {
		if _, err := s.files.mcpRead(p); err == nil {
			t.Errorf("read followed %s", p)
		}
		if err := s.files.mcpWrite(p, "changed"); err == nil {
			t.Errorf("write followed %s", p)
		}
		if err := s.files.mcpDelete(p); err == nil {
			t.Errorf("delete followed %s", p)
		}
	}
	if got, err := s.files.Read("db/secret.container"); err != nil || got != "secret" {
		t.Fatalf("target changed: %q %v", got, err)
	}
}

func TestClientAddr(t *testing.T) {
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.RemoteAddr = "192.168.178.126:51234"
	if got := clientAddr(r); got != "192.168.178.126" {
		t.Fatalf("direct: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "100.64.0.7, 10.0.0.1")
	if got := clientAddr(r); got != "100.64.0.7 via 192.168.178.126" {
		t.Fatalf("proxied: %q", got)
	}
}

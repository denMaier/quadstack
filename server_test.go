package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type fakeSystemdClient struct {
	action func(context.Context, string, string) error
	reload func(context.Context) error
	active []string
}

func (f *fakeSystemdClient) Status(_ context.Context, units []string) (map[string]UnitState, error) {
	out := map[string]UnitState{}
	for _, u := range units {
		if slices.Contains(f.active, u) {
			out[u] = UnitState{Active: "active"}
		}
	}
	return out, nil
}
func (f *fakeSystemdClient) Action(ctx context.Context, unit, action string) error {
	if f.action != nil {
		return f.action(ctx, unit, action)
	}
	return nil
}
func (f *fakeSystemdClient) Reload(ctx context.Context) error {
	if f.reload != nil {
		return f.reload(ctx)
	}
	return nil
}

func testServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	cfg := &Config{
		PublicURL:     "https://qs.example.com",
		QuadletDir:    t.TempDir(),
		StateDir:      t.TempDir(),
		SessionSecret: "test-secret",
		MCPMaxTTL:     time.Hour,
	}
	s := &Server{
		cfg:     cfg,
		auth:    &Authenticator{cfg: cfg},
		files:   &Files{cfg: cfg},
		systemd: &fakeSystemdClient{},
		podman:  newPodman(t.TempDir() + "/missing.sock"),
		static:  fstest.MapFS{"index.html": {Data: []byte("app")}},
		grants:  newGrants(cfg),
		audit:   newAudit(cfg),
	}
	tok, _ := sign(cfg.SessionSecret, sessionData{Sub: "u", Name: "U", Expires: time.Now().Add(time.Hour).Unix()})
	return s, &http.Cookie{Name: sessionCookie, Value: tok}
}

func do(t *testing.T, h http.Handler, c *http.Cookie, method, url, body string, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, url, strings.NewReader(body))
	if c != nil {
		r.AddCookie(c)
	}
	if csrf {
		r.Header.Set("X-Quadstack", "1")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAPI(t *testing.T) {
	s, c := testServer(t)
	h := s.Routes()

	if w := do(t, h, nil, "GET", "/api/stacks", "", false); w.Code != 401 {
		t.Fatalf("no session: %d", w.Code)
	}
	if w := do(t, h, nil, "GET", "/stack/x", "", false); w.Code != 302 {
		t.Fatalf("app without session should redirect to login: %d", w.Code)
	}
	if w := do(t, h, c, "GET", "/anything", "", false); w.Code != 200 || w.Body.String() != "app" {
		t.Fatalf("spa fallback: %d %q", w.Code, w.Body.String())
	}

	// Every endpoint the UI calls answers with JSON, never the app's index.html
	for _, u := range []string{"/api/me", "/api/stacks", "/api/podman-resources", "/api/secrets", "/api/images", "/api/operations", "/api/nope"} {
		w := do(t, h, c, "GET", u, "", false)
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: %d %s", u, w.Code, ct)
		}
	}
	if w := do(t, h, c, "GET", "/api/nope", "", false); w.Code != 404 {
		t.Errorf("unknown endpoint: %d", w.Code)
	}

	file := `{"path":"web/web.container","content":"[Container]\nImage=nginx\n"}`
	if w := do(t, h, c, "POST", "/api/file", file, false); w.Code != 403 {
		t.Fatalf("missing CSRF header: %d", w.Code)
	}
	if w := do(t, h, c, "POST", "/api/file", file, true); w.Code != 200 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, c, "POST", "/api/file", file, true); w.Code != 400 {
		t.Fatalf("create twice should fail: %d", w.Code)
	}
	if w := do(t, h, c, "PUT", "/api/file", `{"path":"web/web.container","content":"[Container]\nImage=nginx:2\n"}`, true); w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, c, "GET", "/api/file?path=../../etc/passwd", "", false); w.Code != 400 {
		t.Fatalf("non-quadlet path should be refused: %d", w.Code)
	}
	if w := do(t, h, c, "PUT", "/api/file", `{"path":"x.service","content":""}`, true); w.Code != 400 {
		t.Fatalf("plain systemd units are not managed: %d", w.Code)
	}

	w := do(t, h, c, "GET", "/api/stacks", "", false)
	var res struct {
		Stacks []struct {
			ID    string
			Files []QFile
		}
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Stacks) != 1 || res.Stacks[0].ID != "d/web" || res.Stacks[0].Files[0].Image != "nginx:2" {
		t.Fatalf("stacks: %s", w.Body)
	}

	// A new, empty stack folder must list with files: [], which the sidebar iterates
	if w := do(t, h, c, "POST", "/api/stacks", `{"name":"fresh"}`, true); w.Code != 200 {
		t.Fatalf("create stack: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, c, "GET", "/api/stacks", "", false); !strings.Contains(w.Body.String(), `"id":"d/fresh","name":"fresh","dir":"fresh","files":[]`) {
		t.Fatalf("empty stack: %s", w.Body)
	}
	os.Remove(s.files.abs("fresh"))

	w = do(t, h, c, "GET", "/api/history?path=web/web.container", "", false)
	var vs []Version
	json.Unmarshal(w.Body.Bytes(), &vs)
	if len(vs) != 1 {
		t.Fatalf("history: %s", w.Body)
	}

	if w := do(t, h, c, "DELETE", "/api/stacks/d/web", "", true); w.Code != 400 {
		t.Fatalf("non-empty stack folder must not be removed: %d", w.Code)
	}
	w = do(t, h, c, "DELETE", "/api/file?path=web/web.container", "", true)
	operation := responseOperation(t, w)
	if err := waitOperation(t, &s.operations, operation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.files.abs("web/web.container")); err == nil {
		t.Fatal("file should be deleted")
	}
	if w := do(t, h, c, "DELETE", "/api/stacks/d/web", "", true); w.Code != 200 {
		t.Fatalf("remove empty stack: %d %s", w.Code, w.Body)
	}
}

func responseOperation(t *testing.T, response *httptest.ResponseRecorder) Operation {
	t.Helper()
	var result struct{ Operation Operation }
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Operation.ID == "" {
		t.Fatalf("missing operation: %s", response.Body)
	}
	return result.Operation
}

func TestActionContinuesAfterRequest(t *testing.T) {
	server, cookie := testServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	server.systemd = &fakeSystemdClient{action: func(ctx context.Context, unit, action string) error { close(started); <-release; return ctx.Err() }}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/unit-action", strings.NewReader(`{"unit":"web.service","action":"restart"}`)).WithContext(ctx)
	request.AddCookie(cookie)
	request.Header.Set("X-Quadstack", "1")
	response := httptest.NewRecorder()
	server.Routes().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("action response: %d %s", response.Code, response.Body)
	}
	operation := responseOperation(t, response)
	cancel()
	<-started
	active, _ := server.operations.Get(operation.ID)
	if active.State != "running" {
		t.Fatalf("action: %+v", active)
	}
	close(release)
	if err := waitOperation(t, &server.operations, operation.ID); err != nil {
		t.Fatal(err)
	}
	handler := server.Routes()
	if response := do(t, handler, nil, "GET", "/api/operations/"+operation.ID, "", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("operation without auth: %d", response.Code)
	}
	if response := do(t, handler, cookie, "GET", "/api/operations/"+operation.ID, "", false); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"succeeded"`) {
		t.Fatalf("operation status: %d %s", response.Code, response.Body)
	}
	if response := do(t, handler, cookie, "POST", "/api/unit-action", `{"unit":"web.service","action":"invalid"}`, true); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid action: %d", response.Code)
	}
}

func TestDeleteRefusesAfterStopFailure(t *testing.T) {
	server, cookie := testServer(t)
	server.systemd = &fakeSystemdClient{action: func(context.Context, string, string) error { return errors.New("stop failed") }}
	if err := server.files.Create("web/web.container", "[Container]\nImage=nginx\n"); err != nil {
		t.Fatal(err)
	}
	response := do(t, server.Routes(), cookie, "DELETE", "/api/file?path=web/web.container&stop=1", "", true)
	operation := responseOperation(t, response)
	if err := waitOperation(t, &server.operations, operation.ID); err == nil {
		t.Fatal("stop failure was swallowed")
	}
	if _, err := os.Stat(server.files.abs("web/web.container")); err != nil {
		t.Fatal("file was deleted despite stop failure")
	}
	final, _ := server.operations.Get(operation.ID)
	if final.Steps[1].State != "skipped" || final.Steps[2].State != "skipped" {
		t.Fatalf("unsafe follow-up steps: %+v", final)
	}
}

func TestDeployFailureIsTrackedAfterSave(t *testing.T) {
	server, cookie := testServer(t)
	server.systemd = &fakeSystemdClient{reload: func(context.Context) error { return errors.New("generator failed") }}
	response := do(t, server.Routes(), cookie, "POST", "/api/file", `{"path":"web/web.container","content":"[Container]\nImage=nginx\n","apply":"restart"}`, true)
	operation := responseOperation(t, response)
	if err := waitOperation(t, &server.operations, operation.ID); err == nil {
		t.Fatal("reload failure was swallowed")
	}
	if _, err := os.Stat(filepath.Join(server.cfg.QuadletDir, "web/web.container")); err != nil {
		t.Fatal("saved file missing")
	}
	final, _ := server.operations.Get(operation.ID)
	if final.State != "failed" || final.Steps[1].State != "skipped" {
		t.Fatalf("deployment state: %+v", final)
	}
}

func TestStackStateTransitionsAndUnknown(t *testing.T) {
	for _, test := range []struct{ active, expected string }{{"active", "running"}, {"inactive", "stopped"}, {"activating", "busy"}, {"deactivating", "busy"}, {"failed", "failed"}, {"", "unknown"}} {
		if result := stackState([]string{"web.service"}, map[string]UnitState{"web.service": {Active: test.active}}); result != test.expected {
			t.Errorf("%q: got %s, want %s", test.active, result, test.expected)
		}
	}
}

func TestDeleteReportsReloadFailure(t *testing.T) {
	server, cookie := testServer(t)
	server.systemd = &fakeSystemdClient{reload: func(context.Context) error { return errors.New("reload failed after deletion") }}
	if err := server.files.Create("web/web.container", "[Container]\nImage=nginx\n"); err != nil {
		t.Fatal(err)
	}
	response := do(t, server.Routes(), cookie, "DELETE", "/api/file?path=web/web.container", "", true)
	operation := responseOperation(t, response)
	if err := waitOperation(t, &server.operations, operation.ID); err == nil {
		t.Fatal("reload failure was swallowed")
	}
	if _, err := os.Stat(server.files.abs("web/web.container")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file not deleted: %v", err)
	}
	final, _ := server.operations.Get(operation.ID)
	if final.State != "failed" || final.Steps[0].State != "succeeded" || final.Steps[1].State != "failed" {
		t.Fatalf("partial operation result was hidden: %+v", final)
	}
}

func TestDeletedFilesAPI(t *testing.T) {
	s, c := testServer(t)
	h := s.Routes()
	s.files.Write("web/web.env", "PASSWORD=old\n")
	s.files.Delete("web/web.env")
	os.Symlink(t.TempDir(), s.files.abs("linked"))

	w := do(t, h, c, "GET", "/api/history/deleted", "", false)
	var ds []DeletedFile
	if json.Unmarshal(w.Body.Bytes(), &ds); len(ds) != 1 || ds[0].Path != "web/web.env" {
		t.Fatalf("deleted: %s", w.Body)
	}
	if w := do(t, h, c, "DELETE", "/api/history?path=web/web.env", "", false); w.Code != 403 {
		t.Fatalf("forgetting needs the CSRF header: %d", w.Code)
	}
	if w := do(t, h, c, "DELETE", "/api/history?path=web/web.env", "", true); w.Code != 200 {
		t.Fatalf("forget: %d %s", w.Code, w.Body)
	}
	if w := do(t, h, c, "GET", "/api/history/deleted", "", false); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("after forgetting: %s", w.Body)
	}
	if w := do(t, h, c, "GET", "/api/stacks", "", false); !strings.Contains(w.Body.String(), "symlinked files or folders") {
		t.Fatalf("symlinks are reported: %s", w.Body)
	}
}

func TestPlanUnits(t *testing.T) {
	parents := map[string]string{"app.service": "vpn.service", "far.service": "vpn.service", "idle.service": "vpn.service"}
	active := func(u string) bool { return u == "far.service" }
	for _, test := range []struct {
		units  []string
		action string
		want   string
	}{
		// a namespace exists before a container joins it, whatever the file order
		{[]string{"app.service", "vpn.service", "web.service"}, "start", "start vpn.service, start web.service, start app.service"},
		// dependents go first, also the running one in another stack; the stopped one stays stopped
		{[]string{"vpn.service"}, "stop", "stop far.service, stop vpn.service"},
		{[]string{"vpn.service"}, "restart", "stop far.service, restart vpn.service, start far.service"},
		{[]string{"app.service", "vpn.service"}, "restart", "stop app.service, stop far.service, restart vpn.service, start app.service, start far.service"},
		// a dependent alone, and a unit nothing shares, are ordinary
		{[]string{"app.service"}, "restart", "restart app.service"},
		{[]string{"web.service"}, "stop", "stop web.service"},
	} {
		var got []string
		for _, step := range planUnits(test.units, test.action, parents, active) {
			got = append(got, step.action+" "+step.unit)
		}
		if strings.Join(got, ", ") != test.want {
			t.Errorf("%s %v:\n got %s\nwant %s", test.action, test.units, strings.Join(got, ", "), test.want)
		}
	}
}

// vpnServer has a VPN container whose network a container in its stack and one in another stack share
func vpnServer(t *testing.T) (*Server, *http.Cookie, *[]string) {
	t.Helper()
	server, cookie := testServer(t)
	for rel, content := range map[string]string{
		"vpn/zvpn.container":   "[Container]\nImage=vpn\nContainerName=vpn\n",
		"vpn/app.container":    "[Container]\nImage=app\nNetwork=container:vpn\n",
		"other/qbit.container": "[Container]\nImage=qbit\nNetwork=zvpn.container\n",
	} {
		if err := server.files.Create(rel, content); err != nil {
			t.Fatal(err)
		}
	}
	calls := &[]string{}
	server.systemd = &fakeSystemdClient{active: []string{"app.service", "qbit.service"}, action: func(_ context.Context, unit, action string) error {
		*calls = append(*calls, action+" "+unit)
		if action == "restart" && unit == "zvpn.service" && slices.Contains(*calls, "fail") {
			return errors.New("vpn did not come up")
		}
		return nil
	}}
	return server, cookie, calls
}

func TestSharedNetworkActions(t *testing.T) {
	server, cookie, calls := vpnServer(t)
	handler := server.Routes()
	run := func(method, url, body string) Operation {
		t.Helper()
		*calls = (*calls)[:0]
		operation := responseOperation(t, do(t, handler, cookie, method, url, body, true))
		waitOperation(t, &server.operations, operation.ID)
		final, _ := server.operations.Get(operation.ID)
		return final
	}

	run("POST", "/api/unit-action", `{"unit":"zvpn.service","action":"restart"}`)
	if want := []string{"stop app.service", "stop qbit.service", "restart zvpn.service", "start app.service", "start qbit.service"}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("restart of the VPN unit: %v", *calls)
	}
	run("POST", "/api/unit-action", `{"unit":"zvpn.service","action":"stop"}`)
	if want := []string{"stop app.service", "stop qbit.service", "stop zvpn.service"}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("stop of the VPN unit: %v", *calls)
	}
	// app sorts before zvpn, but cannot join a namespace that does not exist yet
	run("POST", "/api/stack-action", `{"stack":"d/vpn","action":"start"}`)
	if want := []string{"start zvpn.service", "start app.service"}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("start of the stack: %v", *calls)
	}
	run("PUT", "/api/file", `{"path":"vpn/zvpn.container","content":"[Container]\nImage=vpn:2\nContainerName=vpn\n","apply":"restart"}`)
	if want := []string{"stop app.service", "stop qbit.service", "restart zvpn.service", "start app.service", "start qbit.service"}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("deploy of the VPN unit: %v", *calls)
	}
}

// Fail closed: when the VPN container did not come back, nothing that shares its network is started
func TestDependentsNotStartedAfterParentFailure(t *testing.T) {
	server, cookie, calls := vpnServer(t)
	*calls = append(*calls, "fail")
	operation := responseOperation(t, do(t, server.Routes(), cookie, "POST", "/api/stack-action", `{"stack":"d/vpn","action":"restart"}`, true))
	if err := waitOperation(t, &server.operations, operation.ID); err == nil {
		t.Fatal("the failed restart was swallowed")
	}
	if want := []string{"fail", "stop app.service", "stop qbit.service", "restart zvpn.service"}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("systemd calls: %v", *calls)
	}
	final, _ := server.operations.Get(operation.ID)
	for _, step := range final.Steps[3:] {
		if step.State != "failed" || !strings.Contains(step.Error, "not attempted") {
			t.Fatalf("dependent step: %+v", step)
		}
	}
}

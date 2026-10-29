package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func testServer(t *testing.T) (*Server, *http.Cookie) {
	t.Helper()
	cfg := &Config{
		PublicURL:     "https://qs.example.com",
		QuadletDir:    t.TempDir(),
		StateDir:      t.TempDir(),
		SessionSecret: "test-secret",
	}
	s := &Server{
		cfg:     cfg,
		auth:    &Authenticator{cfg: cfg},
		files:   &Files{cfg: cfg},
		systemd: &Systemd{},
		static:  fstest.MapFS{"index.html": {Data: []byte("app")}},
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
	for _, u := range []string{"/api/me", "/api/stacks", "/api/podman-resources", "/api/secrets", "/api/images", "/api/nope"} {
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

	w = do(t, h, c, "GET", "/api/history?path=web/web.container", "", false)
	var vs []Version
	json.Unmarshal(w.Body.Bytes(), &vs)
	if len(vs) != 1 {
		t.Fatalf("history: %s", w.Body)
	}

	if w := do(t, h, c, "DELETE", "/api/stacks/d/web", "", true); w.Code != 400 {
		t.Fatalf("non-empty stack folder must not be removed: %d", w.Code)
	}
	do(t, h, c, "DELETE", "/api/file?path=web/web.container", "", true)
	if _, err := os.Stat(s.files.abs("web/web.container")); err == nil {
		t.Fatal("file should be deleted")
	}
	if w := do(t, h, c, "DELETE", "/api/stacks/d/web", "", true); w.Code != 200 {
		t.Fatalf("remove empty stack: %d %s", w.Code, w.Body)
	}
}

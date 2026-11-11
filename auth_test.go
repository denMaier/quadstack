package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestAllowedVerifiedEmail(t *testing.T) {
	a := &Authenticator{cfg: &Config{AllowedUsers: []string{"admin@example.com"}}}
	for _, tc := range []struct {
		c    claims
		want bool
	}{
		{claims{Sub: "other", Email: "admin@example.com"}, false},
		{claims{Sub: "other", Email: "ADMIN@example.com", EmailVerified: true}, true},
		{claims{Sub: "other", PreferredUsername: "admin@example.com"}, false},
		{claims{Sub: "admin@example.com"}, true},
	} {
		if got := a.allowed(tc.c); got != tc.want {
			t.Errorf("allowed(%+v) = %v", tc.c, got)
		}
	}
}

func TestUserInfoSubject(t *testing.T) {
	if sameSubject("", "") || sameSubject("one", "two") || !sameSubject("one", "one") {
		t.Fatal("UserInfo must match the ID token subject")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "/",
		"/stack/d%2Fweb":     "/stack/d%2Fweb",
		"https://evil.com/":  "/",
		"//evil.com/":        "/",
		"/\\evil.com":        "/",
		"javascript:alert()": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeIssuer is a minimal OIDC provider that signs ID tokens with the given claims
type fakeIssuer struct {
	*httptest.Server
	key    *rsa.PrivateKey
	nonce  string         // from the last authorization request
	claims map[string]any // of the next ID token
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token",
			"jwks_uri": f.URL + "/jwks", "end_session_endpoint": f.URL + "/logout",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k"))
		if err != nil {
			t.Error(err)
			return
		}
		c := map[string]any{"iss": f.URL, "aud": "quadstack", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": f.nonce}
		for k, v := range f.claims {
			c[k] = v
		}
		idt, err := jwt.Signed(signer).Claims(c).Serialize()
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idt})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// signIn runs the login and the callback and returns the callback's response
func signIn(t *testing.T, a *Authenticator, f *fakeIssuer, state func(string) string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	a.HandleLogin(w, httptest.NewRequest("GET", "/auth/login?next=/stack/x", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	target, _ := url.Parse(w.Header().Get("Location"))
	q := target.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != "https://qs.example.com/auth/callback" {
		t.Fatalf("authorization request: %s", target)
	}
	f.nonce = q.Get("nonce")
	r := httptest.NewRequest("GET", "/auth/callback?code=c&state="+url.QueryEscape(state(q.Get("state"))), nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	w = httptest.NewRecorder()
	a.HandleCallback(w, r)
	return w
}

func sessionCookieFrom(w *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestSignIn(t *testing.T) {
	f := newFakeIssuer(t)
	a := &Authenticator{cfg: &Config{
		PublicURL: "https://qs.example.com", Issuer: f.URL, ClientID: "quadstack", SessionSecret: "s",
		GroupsClaim: "groups", AllowedGroups: []string{"admins"}, Scopes: []string{"openid", "groups"},
	}}
	same := func(s string) string { return s }

	f.claims = map[string]any{"sub": "u1", "name": "Ada", "groups": []string{"admins"}}
	w := signIn(t, a, f, same)
	c := sessionCookieFrom(w)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/stack/x" || c == nil || !c.Secure || !c.HttpOnly {
		t.Fatalf("allowed: %d %v %+v", w.Code, w.Header(), c)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	sess, ok := a.Session(r)
	if !ok || sess.Sub != "u1" || sess.Name != "Ada" || sess.IDToken == "" {
		t.Fatalf("session: %+v %v", sess, ok)
	}
	// Logout ends the provider's session too, with the ID token as a hint
	w = httptest.NewRecorder()
	a.HandleLogout(w, r)
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, f.URL+"/logout?") || !strings.Contains(loc, "id_token_hint=") {
		t.Fatalf("logout: %s", loc)
	}

	// Many groups make the ID token too large for a cookie; it is left out, and sign-in still works
	var groups []string
	for i := range 300 {
		groups = append(groups, fmt.Sprintf("some-rather-long-group-name-%03d", i))
	}
	f.claims = map[string]any{"sub": "u2", "name": strings.Repeat("N", 500), "groups": append(groups, "admins")}
	w = signIn(t, a, f, same)
	if c = sessionCookieFrom(w); w.Code != http.StatusFound || c == nil || len(c.Value) > maxCookieValue {
		t.Fatalf("large token: %d, cookie %d bytes", w.Code, len(c.String()))
	}
	r = httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	if sess, ok := a.Session(r); !ok || sess.IDToken != "" || len([]rune(sess.Name)) != 80 {
		t.Fatalf("large token session: %v %d %q", ok, len(sess.IDToken), sess.Name)
	}
	w = httptest.NewRecorder()
	a.HandleLogout(w, r)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "client_id=quadstack") || strings.Contains(loc, "id_token_hint") {
		t.Fatalf("logout without hint: %s", loc)
	}

	f.claims = map[string]any{"sub": "u3", "groups": []string{"users"}}
	if w = signIn(t, a, f, same); w.Code != http.StatusForbidden || sessionCookieFrom(w) != nil {
		t.Fatalf("not allowed: %d", w.Code)
	}
	f.claims = map[string]any{"sub": "u1", "groups": []string{"admins"}}
	if w = signIn(t, a, f, func(string) string { return "forged" }); w.Code != http.StatusBadRequest || sessionCookieFrom(w) != nil {
		t.Fatalf("state mismatch: %d", w.Code)
	}
	f.claims = map[string]any{"sub": "u1", "groups": []string{"admins"}, "aud": "another-client"}
	if w = signIn(t, a, f, same); w.Code != http.StatusUnauthorized || sessionCookieFrom(w) != nil {
		t.Fatalf("token for another client: %d", w.Code)
	}
}

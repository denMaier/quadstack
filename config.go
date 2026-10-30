package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is read once at startup from QUADSTACK_* environment variables,
// usually supplied by the systemd unit's EnvironmentFile.
type Config struct {
	Listen    string
	PublicURL string // https://quadstack.example.com, used for the OIDC callback

	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
	GroupsClaim  string

	// A user may sign in when they match AllowedUsers or belong to one of AllowedGroups
	AllowedUsers  []string // matched against sub, email and preferred_username
	AllowedGroups []string

	QuadletDir string // the rootless Quadlet search path, ~/.config/containers/systemd
	StateDir   string // edit history and the session secret
	QuadletBin string // Podman's quadlet generator, used for validation
	PodletBin  string // optional, enables importing docker run / compose
	Terminal   bool   // allow `podman exec` shells from the browser, and the MCP exec tool

	// The longest an MCP grant may last; 0 turns MCP access off entirely
	MCPMaxTTL time.Duration

	SessionSecret string
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func list(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func xdg(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

// quadlet generator locations used by the common distributions
var quadletPaths = []string{
	"/usr/libexec/podman/quadlet",
	"/usr/lib/podman/quadlet",
	"/usr/local/libexec/podman/quadlet",
}

func findQuadlet() string {
	for _, p := range quadletPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func LoadConfig() (*Config, error) {
	c := &Config{
		Listen:        env("QUADSTACK_LISTEN", "127.0.0.1:8090"),
		PublicURL:     strings.TrimRight(env("QUADSTACK_PUBLIC_URL", ""), "/"),
		Issuer:        strings.TrimRight(env("QUADSTACK_OIDC_ISSUER", ""), "/"),
		ClientID:      env("QUADSTACK_OIDC_CLIENT_ID", ""),
		ClientSecret:  env("QUADSTACK_OIDC_CLIENT_SECRET", ""),
		Scopes:        list(env("QUADSTACK_OIDC_SCOPES", "openid profile email groups")),
		GroupsClaim:   env("QUADSTACK_OIDC_GROUPS_CLAIM", "groups"),
		AllowedUsers:  list(env("QUADSTACK_ALLOWED_USERS", "")),
		AllowedGroups: list(env("QUADSTACK_ALLOWED_GROUPS", "")),
		QuadletDir:    env("QUADSTACK_QUADLET_DIR", filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "containers", "systemd")),
		StateDir:      env("QUADSTACK_STATE_DIR", filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "quadstack")),
		QuadletBin:    env("QUADSTACK_QUADLET_BIN", findQuadlet()),
		PodletBin:     env("QUADSTACK_PODLET_BIN", ""),
		Terminal:      env("QUADSTACK_TERMINAL", "true") == "true",
	}
	hours, err := strconv.ParseFloat(env("QUADSTACK_MCP_MAX_HOURS", "24"), 64)
	if err != nil || hours < 0 {
		return nil, errors.New("QUADSTACK_MCP_MAX_HOURS must be a number of hours, 0 to turn MCP access off")
	}
	c.MCPMaxTTL = time.Duration(hours * float64(time.Hour))
	if c.PodletBin == "" {
		c.PodletBin, _ = exec.LookPath("podlet")
	}

	var missing []string
	for k, v := range map[string]string{
		"QUADSTACK_PUBLIC_URL":     c.PublicURL,
		"QUADSTACK_OIDC_ISSUER":    c.Issuer,
		"QUADSTACK_OIDC_CLIENT_ID": c.ClientID,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required settings: %s", strings.Join(missing, ", "))
	}
	// Fail closed: an OIDC provider alone would let every account it knows in
	if len(c.AllowedUsers) == 0 && len(c.AllowedGroups) == 0 {
		return nil, errors.New("set QUADSTACK_ALLOWED_USERS and/or QUADSTACK_ALLOWED_GROUPS")
	}

	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.QuadletDir, 0o755); err != nil {
		return nil, err
	}
	secret, err := loadSecret(filepath.Join(c.StateDir, "session.key"))
	if err != nil {
		return nil, err
	}
	c.SessionSecret = secret
	return c, nil
}

// loadSecret keeps sessions valid across restarts without asking for a secret
func loadSecret(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	s := hex.EncodeToString(buf)
	return s, os.WriteFile(path, []byte(s), 0o600)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testFiles(t *testing.T, quadlet string) *Files {
	t.Helper()
	return &Files{cfg: &Config{QuadletDir: t.TempDir(), StateDir: t.TempDir(), QuadletBin: quadlet}}
}

// fakeQuadlet writes a quadlet generator that runs script
func fakeQuadlet(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "quadlet")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidateFailsClosed(t *testing.T) {
	content := "[Container]\nImage=nginx\n"
	for name, tc := range map[string]struct {
		quadlet string
		timeout time.Duration
		ok      bool
	}{
		// quadlet rejected another file: not this file's problem
		"other file rejected": {fakeQuadlet(t, `echo "quadlet-generator[1]: converting \"zz.container\": bad" >&2; exit 1`), 0, true},
		"this file rejected":  {fakeQuadlet(t, `echo "quadlet-generator[1]: converting \"a.container\": bad" >&2; exit 1`), 0, false},
		"crashed":             {fakeQuadlet(t, `kill -9 $$`), 0, false},
		"cannot run":          {filepath.Join(t.TempDir(), "missing"), 0, false},
		"timed out":           {fakeQuadlet(t, `exec sleep 5`), 200 * time.Millisecond, false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if tc.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			v := testFiles(t, tc.quadlet).Validate(ctx, "a.container", &content)
			if v.OK != tc.ok || (!v.OK && v.Output == "") {
				t.Fatalf("got %+v, want ok=%v with a reason", v, tc.ok)
			}
		})
	}
}

// TestValidateWithQuadlet runs Podman's own generator when it is installed (CI installs it)
func TestValidateWithQuadlet(t *testing.T) {
	bin := findQuadlet()
	if bin == "" {
		t.Skip("Podman's quadlet generator is not installed")
	}
	fl := testFiles(t, bin)
	if err := fl.Write("web.network", "[Network]\n"); err != nil {
		t.Fatal(err)
	}
	good := "[Container]\nImage=docker.io/library/nginx:latest\nNetwork=web.network\n"
	if v := fl.Validate(context.Background(), "web/web.container", &good); !v.OK || !strings.Contains(v.Generated, "podman run") {
		t.Fatalf("good file: %+v", v)
	}
	bad := "[Container]\nImage=docker.io/library/nginx:latest\nBogusKey=1\n"
	if v := fl.Validate(context.Background(), "web/web.container", &bad); v.OK || !strings.Contains(v.Output, "BogusKey") {
		t.Fatalf("bad file: %+v", v)
	}
}

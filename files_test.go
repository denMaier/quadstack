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

func TestMoveKeepsHistory(t *testing.T) {
	fl := testFiles(t, "")
	fl.Create("a.container", "v1")
	fl.Write("a.container", "v2")
	if err := fl.Move("a.container", "web/a.container"); err != nil {
		t.Fatal(err)
	}
	if vs, _ := fl.History("a.container"); len(vs) != 0 {
		t.Fatalf("history left behind: %v", vs)
	}
	vs, _ := fl.History("web/a.container")
	if len(vs) != 2 {
		t.Fatalf("versions after move = %d, want 2 (v1, and v2 kept by the move)", len(vs))
	}
	if c, _ := fl.ReadVersion("web/a.container", vs[0].ID); c != "v2" {
		t.Fatalf("newest version = %q", c)
	}
	if err := fl.Move("web/a.container", "web/a.container"); err == nil {
		t.Fatal("moving onto an existing file must fail")
	}
}

func TestDeletedAndForget(t *testing.T) {
	fl := testFiles(t, "")
	fl.Create("web/web.env", "PASSWORD=old")
	fl.Create("web/web.container", "[Container]\n")
	if err := fl.Delete("web/web.env"); err != nil {
		t.Fatal(err)
	}
	ds, err := fl.Deleted()
	if err != nil || len(ds) != 1 || ds[0].Path != "web/web.env" || ds[0].Versions != 1 {
		t.Fatalf("deleted = %+v, %v", ds, err)
	}
	if err := fl.Forget("web/web.env"); err != nil {
		t.Fatal(err)
	}
	if ds, _ := fl.Deleted(); len(ds) != 0 {
		t.Fatalf("forgotten history still listed: %+v", ds)
	}
	if _, err := os.Stat(fl.historyDir("web/web.env")); !os.IsNotExist(err) {
		t.Fatal("the history directory should be gone")
	}
	// Without any history at all, the list is empty, not an error
	if ds, err := testFiles(t, "").Deleted(); err != nil || ds == nil || len(ds) != 0 {
		t.Fatalf("no history: %v %v", ds, err)
	}
}

func TestSymlinksAreReportedNotManaged(t *testing.T) {
	fl := testFiles(t, "")
	outside := filepath.Join(t.TempDir(), "real.container")
	os.WriteFile(outside, []byte("[Container]\nImage=x\n"), 0o600)
	os.Symlink(outside, fl.abs("linked.container"))
	os.Symlink(t.TempDir(), fl.abs("linkedstack"))
	fl.Create("app.container", "[Container]\nImage=app\n")

	inv, err := Scan(fl.cfg.QuadletDir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(inv.Linked, ",") != "linked.container,linkedstack" || len(inv.Stacks) != 1 {
		t.Fatalf("linked = %v, stacks = %d", inv.Linked, len(inv.Stacks))
	}
	if err := fl.Write("linked.container", "changed"); err == nil {
		t.Fatal("writing must not replace a symlink")
	}
	if err := fl.Delete("linked.container"); err == nil {
		t.Fatal("deleting a symlink must be refused")
	}
	if b, _ := os.ReadFile(outside); string(b) != "[Container]\nImage=x\n" {
		t.Fatal("the link target changed")
	}
	// Validation sees linked files, as quadlet does
	tmp, err := fl.scratchCopy("app.container", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	if _, err := os.Stat(filepath.Join(tmp, "linked.container")); err != nil {
		t.Fatalf("scratch copy lacks the linked file: %v", err)
	}
}

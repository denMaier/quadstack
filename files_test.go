package main

import (
	"context"
	"errors"
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

// Hints are about what the generator cannot see; they warn and never block
func TestValidateHints(t *testing.T) {
	fl := testFiles(t, fakeQuadlet(t, "exit 0"))
	fl.Create("vpn/gluetun.container", "[Container]\nImage=gluetun\nContainerName=gluetun\nPublishPort=5030:5030\n")
	fl.Create("vpn/there.env", "A=1\n")
	check := func(rel, content string) string {
		t.Helper()
		v := fl.Validate(context.Background(), rel, &content)
		if !v.OK {
			t.Fatalf("a hint blocked the save: %+v", v)
		}
		return v.Output
	}

	out := check("vpn/slskd.container", "[Container]\nImage=slskd\nNetwork=gluetun.container\nPublishPort=5030:5030\n"+
		"ContainerName=gluetun\nEnvironmentFile=there.env\nEnvironmentFile=gone.env\nEnvironmentFile=%t/later.env\n")
	for _, want := range []string{
		"PublishPort= has no effect here",
		`vpn/gluetun.container names its container "gluetun" too`,
		"EnvironmentFile=gone.env: " + fl.abs("vpn/gone.env") + " does not exist",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "Warning:") != 3 {
		t.Errorf("want exactly three warnings:\n%s", out)
	}
	// The container that owns the namespace publishes the ports, and an ordinary file gets no hints
	if out := check("vpn/gluetun.container", "[Container]\nImage=gluetun\nContainerName=gluetun\nPublishPort=5030:5030\n"); out != "" {
		t.Errorf("hints for the VPN container: %s", out)
	}
	if out := check("web.container", "[Container]\nImage=nginx\nPublishPort=8080:80\n"); out != "" {
		t.Errorf("hints for an ordinary container: %s", out)
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
	fl.Write("web/web.env", "A=1\n")
	good := "[Container]\nImage=docker.io/library/nginx:latest\nNetwork=web.network\nEnvironmentFile=web.env\n"
	v := fl.Validate(context.Background(), "web/web.container", &good)
	if !v.OK || !strings.Contains(v.Generated, "podman run") {
		t.Fatalf("good file: %+v", v)
	}
	// The generated unit names the real files, not the scratch copy
	if !strings.Contains(v.Generated, "--env-file "+fl.abs("web/web.env")) || strings.Contains(v.Generated, "scratch-") {
		t.Fatalf("generated unit: %s", v.Generated)
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
	outsideDir := t.TempDir()
	os.WriteFile(filepath.Join(outsideDir, "there.container"), []byte("[Container]\nImage=y\n"), 0o600)
	os.Symlink(outsideDir, fl.abs("linkedstack"))
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
	// A linked folder is not a way out of the Quadlet directory either
	if err := fl.Write("linkedstack/new.container", "new"); !errors.Is(err, errLinked) {
		t.Fatalf("writing through a linked folder: %v", err)
	}
	if _, err := fl.Read("linkedstack/there.container"); !errors.Is(err, errLinked) {
		t.Fatalf("reading through a linked folder: %v", err)
	}
	if err := fl.Move("app.container", "linkedstack/app.container"); !errors.Is(err, errLinked) {
		t.Fatalf("moving into a linked folder: %v", err)
	}
	if err := fl.Delete("linkedstack/there.container"); !errors.Is(err, errLinked) {
		t.Fatalf("deleting through a linked folder: %v", err)
	}
	if entries, _ := os.ReadDir(outsideDir); len(entries) != 1 {
		t.Fatalf("the linked folder changed: %v", entries)
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

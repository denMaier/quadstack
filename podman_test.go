package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakePodman puts a podman script on PATH that answers like Podman 5.4
func fakePodman(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "podman"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestSecrets(t *testing.T) {
	fakePodman(t, `case "$2" in
ls) printf 'ea21917e5434f3be6fb12d65f\n' ;;
inspect) [ "$3" = ea21917e5434f3be6fb12d65f ] && cat <<'EOF'
[{"ID":"ea21917e5434f3be6fb12d65f","CreatedAt":"2026-10-29T14:03:57.674606236+01:00","UpdatedAt":"2026-10-29T14:03:57.674606236+01:00",
  "Spec":{"Name":"BETTER-AUTH-SECRET","Driver":{"Name":"file","Options":{"path":"/x"}},"Labels":{}}}]
EOF
;;
esac
`)
	ss, err := Secrets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].Spec.Name != "BETTER-AUTH-SECRET" || ss[0].Spec.Driver.Name != "file" || ss[0].CreatedAt == "" {
		t.Fatalf("secrets = %+v", ss)
	}
}

func TestSecretsEmpty(t *testing.T) {
	// inspect without IDs would fail, so it must not be called
	fakePodman(t, `[ "$2" = ls ] && exit 0; exit 1`)
	ss, err := Secrets(context.Background())
	if err != nil || len(ss) != 0 {
		t.Fatalf("secrets = %v, %v", ss, err)
	}
}

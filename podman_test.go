package main

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestSecrets(t *testing.T) {
	podman := fakePodmanAPI(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `[{"ID":"ea21917e5434f3be6fb12d65f","CreatedAt":"2026-10-29T14:03:57.674606236+01:00","Spec":{"Name":"BETTER-AUTH-SECRET","Driver":{"Name":"file"}}}]`)
	})
	ss, err := podman.Secrets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 1 || ss[0].Spec.Name != "BETTER-AUTH-SECRET" || ss[0].Spec.Driver.Name != "file" || ss[0].CreatedAt == "" {
		t.Fatalf("secrets = %+v", ss)
	}
}

func TestSecretsEmpty(t *testing.T) {
	podman := fakePodmanAPI(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `[]`) })
	ss, err := podman.Secrets(context.Background())
	if err != nil || len(ss) != 0 {
		t.Fatalf("secrets = %v, %v", ss, err)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fakePodmanAPI(t *testing.T, handler http.HandlerFunc) *Podman {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "qs-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(directory) })
	socket := filepath.Join(directory, "podman.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener)
	client := newPodman(socket)
	t.Cleanup(func() { client.client.CloseIdleConnections(); server.Close() })
	return client
}

func TestPodmanListsOverUnixAPI(t *testing.T) {
	podman := fakePodmanAPI(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("method: %s", request.Method)
		}
		switch request.URL.Path {
		case podmanAPIPath + "/containers/json":
			if request.URL.Query().Get("all") != "true" || request.URL.Query().Get("pod") != "true" {
				t.Errorf("container options: %s", request.URL.RawQuery)
			}
			io.WriteString(writer, `[{"Id":"container-id","Names":["web"],"State":"running","PodName":"app","Labels":{"PODMAN_SYSTEMD_UNIT":"web.service"}}]`)
		case podmanAPIPath + "/images/json":
			io.WriteString(writer, `[{"Id":"image-id","Names":["nginx:latest"],"Size":1024,"Created":1234}]`)
		case podmanAPIPath + "/networks/json":
			io.WriteString(writer, `[{"name":"app","driver":"bridge","dns_enabled":true}]`)
		case podmanAPIPath + "/volumes/json":
			io.WriteString(writer, `[{"Name":"data","Driver":"local","Mountpoint":"/data"}]`)
		default:
			t.Errorf("unexpected endpoint: %s", request.URL.Path)
			http.NotFound(writer, request)
		}
	})
	containers, err := podman.Containers(context.Background())
	if err != nil || len(containers) != 1 || containers[0].Unit() != "web.service" || containers[0].Pod != "app" {
		t.Fatalf("containers: %+v, %v", containers, err)
	}
	images, err := podman.Images(context.Background())
	if err != nil || len(images) != 1 || images[0].Size != 1024 {
		t.Fatalf("images: %+v, %v", images, err)
	}
	networks, err := podman.Networks(context.Background())
	if err != nil || len(networks) != 1 || !networks[0].DNS {
		t.Fatalf("networks: %+v, %v", networks, err)
	}
	volumes, err := podman.Volumes(context.Background())
	if err != nil || len(volumes) != 1 || volumes[0].Name != "data" {
		t.Fatalf("volumes: %+v, %v", volumes, err)
	}
}

func TestPodmanSecretWrites(t *testing.T) {
	value := "secret\x00value\n"
	podman := fakePodmanAPI(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			data, _ := io.ReadAll(request.Body)
			if string(data) != value || request.URL.Path != podmanAPIPath+"/secrets/create" || request.URL.Query().Get("replace") != "true" || request.URL.Query().Get("name") != "APP_KEY" {
				t.Errorf("secret request: %s %s, body length=%d", request.Method, request.URL, len(data))
			}
			writer.WriteHeader(http.StatusCreated)
			io.WriteString(writer, `{"ID":"secret-id"}`)
		} else if request.Method == http.MethodDelete {
			if request.URL.Path != podmanAPIPath+"/secrets/APP_KEY" {
				t.Errorf("secret path: %s", request.URL.Path)
			}
			writer.WriteHeader(http.StatusNoContent)
		} else {
			t.Errorf("unexpected method: %s", request.Method)
		}
	})
	if err := podman.SetSecret(context.Background(), "APP_KEY", []byte(value), true); err != nil {
		t.Fatal(err)
	}
	if err := podman.RemoveSecret(context.Background(), "APP_KEY"); err != nil {
		t.Fatal(err)
	}
	if err := podman.SetSecret(context.Background(), "../bad", []byte(value), false); err == nil {
		t.Fatal("invalid secret name accepted")
	}
}

func TestPodmanAPIFailures(t *testing.T) {
	for _, test := range []struct {
		name           string
		status         int
		body, expected string
	}{
		{"http-error", 500, `{"message":"storage is locked"}`, "storage is locked"},
		{"invalid-json", 200, `<html>proxy error</html>`, "decoding Podman API"},
		{"redirect", 302, `redirect`, "HTTP 302"},
	} {
		t.Run(test.name, func(t *testing.T) {
			podman := fakePodmanAPI(t, func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Location", "http://example.invalid")
				writer.WriteHeader(test.status)
				io.WriteString(writer, test.body)
			})
			_, err := podman.Containers(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.expected) {
				t.Fatalf("error: %v", err)
			}
		})
	}
	podman := newPodman(filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := podman.Containers(context.Background()); err == nil || !strings.Contains(err.Error(), "podman.socket") {
		t.Fatalf("missing socket error: %v", err)
	}
}

func TestPodmanRequestDeadline(t *testing.T) {
	podman := fakePodmanAPI(t, func(writer http.ResponseWriter, request *http.Request) { <-request.Context().Done() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := podman.Containers(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline: %v", err)
	}
}

func TestPodmanPullStream(t *testing.T) {
	for _, test := range []struct {
		name, body string
		succeeds   bool
	}{
		{"success", "{\"stream\":\"Downloading layer\\n\"}\n{\"images\":[\"image-id\"]}\n", true},
		{"stream-error", `{"error":"registry denied access"}`, false},
		{"truncated", "{\"stream\":\"Downloading\"}\n{", false},
		{"incomplete", `{"stream":"Downloading"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			podman := fakePodmanAPI(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != podmanAPIPath+"/images/pull" || request.URL.Query().Get("reference") != "registry.example/app:tag" {
					t.Errorf("pull request: %s %s", request.Method, request.URL)
				}
				io.WriteString(writer, test.body)
			})
			var progress bytes.Buffer
			images, err := podman.Pull(context.Background(), "registry.example/app:tag", &progress)
			if (err == nil) != test.succeeds {
				t.Fatalf("pull: %v, %v", images, err)
			}
			if test.succeeds && (len(images) != 1 || images[0] != "image-id" || progress.Len() == 0) {
				t.Fatalf("pull result: %v, %q", images, progress.String())
			}
		})
	}
}

func execFrame(stream byte, message string) []byte {
	frame := make([]byte, 8, 8+len(message))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:], uint32(len(message)))
	return append(frame, message...)
}

func TestPodmanExecAPI(t *testing.T) {
	podman := fakePodmanAPI(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case podmanAPIPath + "/containers/web/exec":
			writer.WriteHeader(http.StatusCreated)
			io.WriteString(writer, `{"Id":"exec-id"}`)
		case podmanAPIPath + "/exec/exec-id/start":
			connection, buffer, err := writer.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer connection.Close()
			fmt.Fprint(buffer, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			buffer.Write(execFrame(1, "output"))
			buffer.Write(execFrame(2, "failure"))
			buffer.Flush()
		case podmanAPIPath + "/exec/exec-id/json":
			io.WriteString(writer, `{"Running":false,"ExitCode":7}`)
		default:
			t.Errorf("unexpected exec request: %s", request.URL)
			http.NotFound(writer, request)
		}
	})
	var stdout, stderr bytes.Buffer
	code, err := podman.Exec(context.Background(), "web", []string{"sh", "-c", "echo output"}, &stdout, &stderr)
	if err != nil || code != 7 || stdout.String() != "output" || stderr.String() != "failure" {
		t.Fatalf("exec: %d %q %q %v", code, stdout.String(), stderr.String(), err)
	}
}

func TestPodmanExecFraming(t *testing.T) {
	for _, input := range [][]byte{{1, 0}, {9, 0, 0, 0, 0, 0, 0, 0}, execFrame(1, "hello")[:10]} {
		if err := copyExecOutput(bytes.NewReader(input), io.Discard, io.Discard); err == nil {
			t.Fatalf("invalid frame accepted: %v", input)
		}
	}
}

func TestPodmanMutationsAreNotRetried(t *testing.T) {
	var calls atomic.Int32
	podman := fakePodmanAPI(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			io.WriteString(writer, `[]`)
			return
		}
		calls.Add(1)
		io.Copy(io.Discard, request.Body)
		connection, _, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close()
	})
	if _, err := podman.Containers(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := podman.SetSecret(context.Background(), "APP_KEY", []byte("value"), false)
	if !errors.Is(err, errOutcomeUnknown) || calls.Load() != 1 {
		t.Fatalf("mutation retried or outcome hidden: calls=%d, error=%v", calls.Load(), err)
	}
}

func TestPodmanExecCancellation(t *testing.T) {
	podman := fakePodmanAPI(t, func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/start") {
			connection, buffer, err := writer.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer connection.Close()
			fmt.Fprint(buffer, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			buffer.Flush()
			io.Copy(io.Discard, connection)
			return
		}
		io.WriteString(writer, `{"Id":"exec-id"}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := podman.Exec(ctx, "web", []string{"sleep", "300"}, io.Discard, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exec did not stop waiting on cancellation: %v", err)
	}
}

func TestPodmanEmptyLists(t *testing.T) {
	podman := fakePodmanAPI(t, func(writer http.ResponseWriter, request *http.Request) { io.WriteString(writer, `null`) })
	images, err := podman.Images(context.Background())
	if err != nil || images == nil || len(images) != 0 {
		t.Fatalf("empty images: %v, %v", images, err)
	}
}

func TestPodmanSocketConfiguration(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	if socket := defaultPodmanSocket(); socket != "/run/user/1234/podman/podman.sock" {
		t.Fatalf("default socket: %s", socket)
	}
	for _, socket := range []string{"tcp://localhost:1234", "http://localhost:1234", "relative.sock"} {
		t.Setenv("QUADSTACK_PODMAN_SOCKET", socket)
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "local Unix socket") {
			t.Fatalf("unsafe endpoint %q accepted: %v", socket, err)
		}
	}
}

func TestPodmanInteractiveExecAndResize(t *testing.T) {
	podman := fakePodmanAPI(t, func(writer http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		switch {
		case strings.HasSuffix(request.URL.Path, "/start"):
			connection, buffer, err := writer.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer connection.Close()
			fmt.Fprint(buffer, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			buffer.Flush()
			input := make([]byte, 5)
			if _, err := io.ReadFull(buffer, input); err != nil {
				t.Error(err)
				return
			}
			connection.Write(input)
		case strings.HasSuffix(request.URL.Path, "/resize"):
			if request.URL.Query().Get("w") != "120" || request.URL.Query().Get("h") != "40" {
				t.Errorf("resize: %s", request.URL)
			}
			writer.WriteHeader(http.StatusCreated)
		default:
			io.WriteString(writer, `{"Id":"exec-id"}`)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	session, err := podman.CreateExec(ctx, "web", []string{"sh"}, true)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := session.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := io.WriteString(stream, "hello"); err != nil {
		t.Fatal(err)
	}
	output := make([]byte, 5)
	if _, err := io.ReadFull(stream, output); err != nil || string(output) != "hello" {
		t.Fatalf("interactive stream: %q, %v", output, err)
	}
	if err := session.Resize(ctx, 120, 40); err != nil {
		t.Fatal(err)
	}
}

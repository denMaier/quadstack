package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func installerSection(t *testing.T, installer, start, end string) string {
	t.Helper()
	startIndex := strings.Index(installer, start)
	if startIndex < 0 {
		t.Fatalf("installer section %q not found", start)
	}
	section := installer[startIndex:]
	endIndex := strings.Index(section, end)
	if endIndex < 0 {
		t.Fatalf("installer section end %q not found", end)
	}
	return section[:endIndex]
}

func TestInstallerReleaseSelection(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is unavailable (used to resolve the preview channel)")
	}
	installer, err := os.ReadFile("deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	helpers := installerSection(t, string(installer), "valid_release_tag() {", "install_binary() {")
	const latestPreview = "v0.7.0-preview.20261114170000.g0123456789ab"
	const oldPreview = "v0.7.0-preview.20261113170000.gabcdef012345"
	const releases = `[
{"tag_name":"v0.7.0","draft":false,"prerelease":false,"created_at":"2026-11-14T18:00:00Z"},
{"tag_name":"v0.7.0-preview.20261114170000.g0123456789ab","draft":false,"prerelease":true,"created_at":"2026-11-14T17:00:00Z","published_at":"2026-11-14T17:01:00Z"},
{"tag_name":"v0.7.0-preview.20261113170000.gabcdef012345","draft":false,"prerelease":true,"created_at":"2026-11-13T17:00:00Z","published_at":"2026-11-14T19:00:00Z"},
{"tag_name":"v0.8.0-preview.20261115170000.gabcdef012345","draft":true,"prerelease":true,"created_at":"2026-11-15T17:00:00Z"},
{"tag_name":"v0.8.0-rc.1","draft":false,"prerelease":true,"created_at":"2026-11-16T17:00:00Z"}
]`
	const harness = `
REPO=denMaier/quadstack
die() { printf '%s\n' "$1" >&2; exit 1; }
msg_info() { :; }
menu() { printf -v "$1" '%s' "$3"; }
curl() {
  printf '%s\n' "${!#}" >>"$INSTALLER_TEST_REQUESTS"
  if [[ $INSTALLER_TEST_FAILURE == 1 ]]; then return 22; fi
  if [[ ${!#} == */releases/latest ]]; then
    printf '%s\n' "$INSTALLER_TEST_REDIRECT"
  else
    printf '%s\n' "$INSTALLER_TEST_RELEASES"
  fi
}
select_version
tag=$(resolve_version)
printf '%s\n' "$tag"
`
	for _, test := range []struct {
		name, current, selected, redirect, response, want, errorText string
		failure                                                      bool
		requests                                                     int
	}{
		{name: "new install defaults to stable", want: "v0.7.0", requests: 1},
		{name: "stable stays stable", current: "v0.6.1", want: "v0.7.0", requests: 1},
		{name: "preview stays preview", current: oldPreview, want: latestPreview, requests: 1},
		{name: "opt into preview", selected: "preview", want: latestPreview, requests: 1},
		{name: "return to stable", current: oldPreview, selected: "stable", want: "v0.7.0", requests: 1},
		{name: "latest alias stays stable", current: oldPreview, selected: "latest", want: "v0.7.0", requests: 1},
		{name: "pin stable", selected: "v0.6.1", want: "v0.6.1"},
		{name: "pin preview", selected: oldPreview, want: oldPreview},
		{name: "reject branch", selected: "main", errorText: "QS_VERSION must"},
		{name: "reject path", selected: "../main", errorText: "QS_VERSION must"},
		{name: "reject leading zeros", selected: "v01.2.3", errorText: "QS_VERSION must"},
		{name: "reject shell syntax", selected: "$(touch forbidden)", errorText: "QS_VERSION must"},
		{name: "empty preview channel", selected: "preview", response: "[]", errorText: "Could not find a published preview", requests: 1},
		{name: "malformed API response", selected: "preview", response: "not json", errorText: "Could not find a published preview", requests: 1},
		{name: "API failure", selected: "preview", failure: true, errorText: "Could not find a published preview", requests: 1},
		{name: "stable cannot resolve to preview", redirect: "https://github.com/denMaier/quadstack/releases/tag/" + oldPreview, errorText: "stable release resolved to a preview", requests: 1},
		{name: "missing stable release", redirect: "https://github.com/denMaier/quadstack/releases", errorText: "Could not find the latest stable", requests: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			requests := filepath.Join(workspace, "requests")
			if err := os.WriteFile(requests, nil, 0600); err != nil {
				t.Fatal(err)
			}
			response, redirect := test.response, test.redirect
			if response == "" {
				response = releases
			}
			if redirect == "" {
				redirect = "https://github.com/denMaier/quadstack/releases/tag/v0.7.0"
			}
			failure := ""
			if test.failure {
				failure = "1"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, bashPath, "-c", "set -Eeuo pipefail\n"+helpers+harness)
			command.Dir = workspace
			command.Env = append(os.Environ(), "QS_VERSION="+test.selected, "CUR="+test.current,
				"INSTALLER_TEST_REQUESTS="+requests, "INSTALLER_TEST_REDIRECT="+redirect,
				"INSTALLER_TEST_RELEASES="+response, "INSTALLER_TEST_FAILURE="+failure)
			output, err := command.CombinedOutput()
			if test.errorText != "" {
				if err == nil || !strings.Contains(string(output), test.errorText) {
					t.Fatalf("wanted %q, got %s, %v", test.errorText, output, err)
				}
			} else if err != nil || strings.TrimSpace(string(output)) != test.want {
				t.Fatalf("version = %q, %v; want %s", output, err, test.want)
			}
			calls, err := os.ReadFile(requests)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(calls), "\n"); got != test.requests {
				t.Fatalf("requests = %d, want %d", got, test.requests)
			}
			if _, err := os.Stat(filepath.Join(workspace, "forbidden")); err == nil {
				t.Fatal("version executed shell syntax")
			}
		})
	}
}

func TestInstallerUpdate(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	installer, err := os.ReadFile("deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile("deploy/quadstack.service")
	if err != nil {
		t.Fatal(err)
	}
	helpers := installerSection(t, string(installer), "user_home() {", "# podlet turns")
	update := installerSection(t, string(installer), "if [[ -n $EXISTING ]]; then", "### Podman")
	const harness = `
REPO=denMaier/quadstack
EXISTING=quadlets
QS_MODE=update
user_home() { printf '%s\n' "$INSTALLER_TEST_HOME"; }
msg_info() { :; }
msg_ok() { :; }
msg_warn() { :; }
menu() { :; }
resolve_version() { printf '%s\n' v9.8.7; }
install_binary() { printf 'binary %s %s\n' "$1" "$2" >>"$INSTALLER_TEST_EVENTS"; }
install_podlet() { :; }
shell_env() { :; }
userdir() {
  [[ $1 == quadlets && $2 == 0755 ]]
  shift 2
  mkdir -p "$@"
}
id() {
  [[ $# == 2 && $1 == -gn && $2 == quadlets ]]
  printf '%s\n' quadlets
}
curl() {
  [[ $# == 4 && $1 == -fsSL && $2 == -o ]]
  printf 'download %s\n' "$4" >>"$INSTALLER_TEST_EVENTS"
  if [[ $INSTALLER_TEST_FAILURE == download ]]; then return 1; fi
  cp "$INSTALLER_TEST_UNIT" "$3"
}
install() {
  [[ $# == 8 && $1 == -o && $2 == quadlets && $3 == -g && $4 == quadlets && $5 == -m && $6 == 0644 ]]
  [[ $8 == "$INSTALLER_TEST_HOME/.config/systemd/user/quadstack.service" ]]
  printf 'install %s %s %s\n' "$2" "$4" "$6" >>"$INSTALLER_TEST_EVENTS"
  if [[ $INSTALLER_TEST_FAILURE == install ]]; then return 1; fi
  cp "$7" "$8"
}
as_user() {
  printf '%s\n' "$*" >>"$INSTALLER_TEST_EVENTS"
  if [[ $* == 'quadlets systemctl --user daemon-reload' && $INSTALLER_TEST_FAILURE == reload ]]; then return 1; fi
  if [[ $* == 'quadlets systemctl --user enable --now --quiet podman.socket' && $INSTALLER_TEST_FAILURE == socket ]]; then return 1; fi
  return 0
}
wait_active() { printf 'active %s\n' "$1" >>"$INSTALLER_TEST_EVENTS"; }
`
	expectedEvents := []string{
		"binary quadlets v9.8.7",
		"download https://raw.githubusercontent.com/denMaier/quadstack/v9.8.7/deploy/quadstack.service",
		"install quadlets quadlets 0644",
		"quadlets systemctl --user daemon-reload",
		"quadlets systemctl --user enable --now --quiet podman.socket",
		"quadlets systemctl --user restart quadstack.service",
		"active quadlets",
	}
	for _, testCase := range []struct {
		name       string
		failure    string
		eventCount int
		updated    bool
		attempts   int
	}{
		{name: "success", eventCount: len(expectedEvents), updated: true, attempts: 1},
		{name: "repeat update", eventCount: len(expectedEvents), updated: true, attempts: 2},
		{name: "download failure", failure: "download", eventCount: 2, attempts: 1},
		{name: "install failure", failure: "install", eventCount: 3, attempts: 1},
		{name: "reload failure", failure: "reload", eventCount: 4, updated: true, attempts: 1},
		{name: "socket failure", failure: "socket", eventCount: 5, updated: true, attempts: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := t.TempDir()
			home := filepath.Join(workspace, "quadlets home")
			eventsPath := filepath.Join(workspace, "events")
			unitPath := filepath.Join(home, ".config/systemd/user/quadstack.service")
			fixturePath := filepath.Join(workspace, "release.service")
			preserved := map[string]string{
				".config/quadstack/quadstack.env":                     "QUADSTACK_OIDC_CLIENT_SECRET=keep-me\n",
				".config/systemd/user/quadstack.service.d/local.conf": "[Service]\nEnvironment=LOCAL_SETTING=keep-me\n",
				".config/containers/systemd/web.container":            "[Container]\nImage=example/web\n",
				".local/state/quadstack/history/previous":             "saved history\n",
			}
			for relativePath, content := range preserved {
				path := filepath.Join(home, relativePath)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			const oldUnit = "[Unit]\nDescription=old service\n"
			if err := os.WriteFile(unitPath, []byte(oldUnit), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fixturePath, unit, 0644); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < testCase.attempts; attempt++ {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				command := exec.CommandContext(ctx, bashPath, "-c", "set -Eeuo pipefail\n"+helpers+harness+update)
				command.Dir = workspace
				command.Env = append(os.Environ(),
					"INSTALLER_TEST_HOME="+home,
					"INSTALLER_TEST_EVENTS="+eventsPath,
					"INSTALLER_TEST_UNIT="+fixturePath,
					"INSTALLER_TEST_FAILURE="+testCase.failure,
					"TMPDIR="+workspace,
				)
				output, err := command.CombinedOutput()
				cancel()
				if (err != nil) != (testCase.failure != "") {
					t.Fatalf("update error = %v, expected failure %q; output:\n%s", err, testCase.failure, output)
				}
			}
			events, err := os.ReadFile(eventsPath)
			if err != nil {
				t.Fatal(err)
			}
			wantEvents := strings.Repeat(strings.Join(expectedEvents[:testCase.eventCount], "\n")+"\n", testCase.attempts)
			if string(events) != wantEvents {
				t.Fatalf("update events:\n%s\nwant:\n%s", events, wantEvents)
			}
			installedUnit, err := os.ReadFile(unitPath)
			if err != nil {
				t.Fatal(err)
			}
			wantUnit := oldUnit
			if testCase.updated {
				wantUnit = string(unit)
			}
			if string(installedUnit) != wantUnit {
				t.Fatalf("installed unit = %q, want %q", installedUnit, wantUnit)
			}
			for relativePath, content := range preserved {
				actual, err := os.ReadFile(filepath.Join(home, relativePath))
				if err != nil || string(actual) != content {
					t.Errorf("%s was not preserved: %q, %v", relativePath, actual, err)
				}
			}
		})
	}
}

func TestInstallerListenSelection(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	installer, err := os.ReadFile("deploy/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	proxmox, err := os.ReadFile("deploy/proxmox/install/quadstack-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	asks := installerSection(t, string(installer), "ask() {", "ask_secret() {")
	environment := installerSection(t, string(installer), "envget() {", "resolve_version() {")
	validation := installerSection(t, string(installer), "check_listen() {", "check_hours() {")
	prompt := installerSection(t, string(installer), "  ask_valid check_listen", "\n\n")
	wrapper := installerSection(t, string(proxmox), "QS_FRESH=1", "\nmotd_ssh")
	const harness = `
BT=Quadstack
tty_ui() { return 0; }
die() { printf '%s\n' "$1" >&2; exit 1; }
cancelled() { die Cancelled; }
whiptail() {
  printf '%s\n' "$6" >>"$INSTALLER_TEST_PROMPTS"
  printf '%s\n' "${INSTALLER_TEST_ANSWER:-${!#}}" >&2
}
ss() {
  if [[ $INSTALLER_TEST_BUSY == 1 ]]; then printf 'LISTEN\n'; fi
}
curl() { printf '%s\n' "$INSTALLER_TEST_PROMPT"; }
bash() {
  [[ $# == 2 && $1 == -c ]]
  eval "$2"
  envline QUADSTACK_LISTEN "$QS_LISTEN"
}
`
	for _, testCase := range []struct {
		name       string
		saved      string
		provided   string
		answer     string
		unattended bool
		busy       bool
		expected   string
		prompts    int
		errorText  string
	}{
		{name: "interactive defaults", expected: "0.0.0.0:8090", prompts: 1},
		{name: "custom port", answer: "0.0.0.0:9080", expected: "0.0.0.0:9080", prompts: 1},
		{name: "loopback", answer: "127.0.0.1:9081", expected: "127.0.0.1:9081", prompts: 1},
		{name: "specific interface", answer: "192.0.2.10:9082", expected: "192.0.2.10:9082", prompts: 1},
		{name: "IPv6", answer: "[::]:9083", expected: "[::]:9083", prompts: 1},
		{name: "keep existing", saved: "127.0.0.1:8181", expected: "127.0.0.1:8181", prompts: 1},
		{name: "change existing", saved: "127.0.0.1:8181", answer: "0.0.0.0:9084", expected: "0.0.0.0:9084", prompts: 1},
		{name: "unattended defaults", unattended: true, expected: "0.0.0.0:8090"},
		{name: "environment override", provided: "127.0.0.1:9085", expected: "127.0.0.1:9085"},
		{name: "trim whitespace", provided: " 0.0.0.0:9086 ", expected: "0.0.0.0:9086"},
		{name: "highest port", provided: "0.0.0.0:65535", expected: "0.0.0.0:65535"},
		{name: "zero port", provided: "0.0.0.0:0", unattended: true, errorText: "listen address"},
		{name: "out of range", provided: "0.0.0.0:65536", unattended: true, errorText: "listen address"},
		{name: "missing port", provided: "0.0.0.0", unattended: true, errorText: "listen address"},
		{name: "busy port", provided: "0.0.0.0:9087", unattended: true, busy: true, errorText: "already in use"},
		{name: "own current port", saved: "0.0.0.0:9088", unattended: true, busy: true, expected: "0.0.0.0:9088"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace := t.TempDir()
			envPath := filepath.Join(workspace, "quadstack.env")
			promptsPath := filepath.Join(workspace, "prompts")
			if err := os.WriteFile(envPath, []byte("QUADSTACK_LISTEN=\""+testCase.saved+"\"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(promptsPath, nil, 0600); err != nil {
				t.Fatal(err)
			}
			unattended := ""
			if testCase.unattended {
				unattended = "1"
			}
			busy := ""
			if testCase.busy {
				busy = "1"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, bashPath, "-c", "set -Eeuo pipefail\n"+asks+environment+validation+harness+wrapper)
			command.Dir = workspace
			command.Env = append(os.Environ(),
				"ENVF="+envPath,
				"QS_LISTEN="+testCase.provided,
				"QS_YES="+unattended,
				"INSTALLER_TEST_PROMPTS="+promptsPath,
				"INSTALLER_TEST_PROMPT="+prompt,
				"INSTALLER_TEST_ANSWER="+testCase.answer,
				"INSTALLER_TEST_BUSY="+busy,
			)
			output, err := command.CombinedOutput()
			if testCase.errorText != "" {
				if err == nil || !strings.Contains(string(output), testCase.errorText) {
					t.Fatalf("expected error %q, got %v; output:\n%s", testCase.errorText, err, output)
				}
			} else if expected := "QUADSTACK_LISTEN=\"" + testCase.expected + "\""; err != nil || strings.TrimSpace(string(output)) != expected {
				t.Fatalf("listen selection = %q, %v; want %q", output, err, expected)
			}
			prompts, err := os.ReadFile(promptsPath)
			if err != nil {
				t.Fatal(err)
			}
			if count := strings.Count(string(prompts), "\n"); count != testCase.prompts {
				t.Errorf("prompt count = %d, want %d", count, testCase.prompts)
			}
			if testCase.prompts > 0 && !strings.Contains(string(prompts), "Listen address and port") {
				t.Errorf("prompt does not explain address and port selection: %s", prompts)
			}
		})
	}
}

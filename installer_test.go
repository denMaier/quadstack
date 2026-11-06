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

func installerSection(testingT *testing.T, installer, start, end string) string {
	testingT.Helper()
	startIndex := strings.Index(installer, start)
	if startIndex < 0 {
		testingT.Fatalf("installer section %q not found", start)
	}
	section := installer[startIndex:]
	endIndex := strings.Index(section, end)
	if endIndex < 0 {
		testingT.Fatalf("installer section end %q not found", end)
	}
	return section[:endIndex]
}

func TestInstallerUpdate(testingT *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		testingT.Skip("bash is unavailable")
	}
	installer, err := os.ReadFile("deploy/install.sh")
	if err != nil {
		testingT.Fatal(err)
	}
	unit, err := os.ReadFile("deploy/quadstack.service")
	if err != nil {
		testingT.Fatal(err)
	}
	helpers := installerSection(testingT, string(installer), "user_home() {", "# podlet turns")
	update := installerSection(testingT, string(installer), "if [[ -n $EXISTING ]]; then", "### Podman")
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
		testingT.Run(testCase.name, func(testingT *testing.T) {
			workspace := testingT.TempDir()
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
					testingT.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					testingT.Fatal(err)
				}
			}
			const oldUnit = "[Unit]\nDescription=old service\n"
			if err := os.WriteFile(unitPath, []byte(oldUnit), 0644); err != nil {
				testingT.Fatal(err)
			}
			if err := os.WriteFile(fixturePath, unit, 0644); err != nil {
				testingT.Fatal(err)
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
					testingT.Fatalf("update error = %v, expected failure %q; output:\n%s", err, testCase.failure, output)
				}
			}
			events, err := os.ReadFile(eventsPath)
			if err != nil {
				testingT.Fatal(err)
			}
			wantEvents := strings.Repeat(strings.Join(expectedEvents[:testCase.eventCount], "\n")+"\n", testCase.attempts)
			if string(events) != wantEvents {
				testingT.Fatalf("update events:\n%s\nwant:\n%s", events, wantEvents)
			}
			installedUnit, err := os.ReadFile(unitPath)
			if err != nil {
				testingT.Fatal(err)
			}
			wantUnit := oldUnit
			if testCase.updated {
				wantUnit = string(unit)
			}
			if string(installedUnit) != wantUnit {
				testingT.Fatalf("installed unit = %q, want %q", installedUnit, wantUnit)
			}
			for relativePath, content := range preserved {
				actual, err := os.ReadFile(filepath.Join(home, relativePath))
				if err != nil || string(actual) != content {
					testingT.Errorf("%s was not preserved: %q, %v", relativePath, actual, err)
				}
			}
		})
	}
}

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestScan(t *testing.T) {
	root := writeTree(t, map[string]string{
		"arr.pod":                        "[Pod]\nPodName=arr\n",
		"sonarr.container":               "[Container]\nImage=lscr.io/linuxserver/sonarr\nPod=arr.pod\n[Install]\nWantedBy=default.target\n",
		"radarr.container":               "[Container]\nImage=lscr.io/linuxserver/radarr\nPod=arr.pod\n",
		"sonarr.container.d/10-env.conf": "[Container]\nEnvironment=A=1\n",
		"kavita.container":               "[Container]\nImage=kavita\nContainerName=kavita\nServiceName=kavita-app\n",
		"kavita.env":                     "SECRET=x\n",
		"media.network":                  "[Network]\n",
		"web/web.container":              "[Container]\nImage=nginx\n",
		"web/web.volume":                 "[Volume]\n",
		"empty/.keep":                    "",
		"notes.txt":                      "ignored",
		".hidden.container":              "[Container]\n",
	})

	inv, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	stacks := inv.Stacks
	got := map[string][]string{}
	for _, s := range stacks {
		for _, f := range s.Files {
			got[s.ID] = append(got[s.ID], f.Path)
		}
		if len(s.Files) == 0 {
			got[s.ID] = nil
		}
	}
	want := map[string][]string{
		"r/arr":    {"arr.pod", "radarr.container", "sonarr.container", "sonarr.container.d/10-env.conf"},
		"r/kavita": {"kavita.container", "kavita.env"},
		"d/web":    {"web/web.container", "web/web.volume"},
		"d/empty":  nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stacks\n got %v\nwant %v", got, want)
	}

	arr, _ := FindStack(root, "r/arr")
	if u := arr.Units(); !reflect.DeepEqual(u, []string{"arr-pod.service", "radarr.service", "sonarr.service"}) {
		t.Errorf("units = %v", u)
	}
	if !arr.Files[2].Autostart || arr.Files[1].Autostart {
		t.Error("autostart should follow [Install]")
	}
	kav, _ := FindStack(root, "r/kavita")
	if f := kav.Files[0]; f.Unit != "kavita-app.service" || f.ContainerName != "kavita" {
		t.Errorf("kavita = %+v", f)
	}
	web, _ := FindStack(root, "d/web")
	if f := web.Files[0]; f.ContainerName != "systemd-web" || f.Unit != "web.service" {
		t.Errorf("web = %+v", f)
	}
}

func TestCleanRel(t *testing.T) {
	for in, want := range map[string]string{
		"a.container":           "a.container",
		"/web/a.container":      "web/a.container",
		"../../etc/passwd":      "etc/passwd",
		"web/../../a.container": "a.container",
	} {
		if got, err := cleanRel(in); err != nil || got != want {
			t.Errorf("cleanRel(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/", ".config/x.container", "web/.git/config"} {
		if _, err := cleanRel(bad); err == nil {
			t.Errorf("cleanRel(%q) should fail", bad)
		}
	}
}

func TestShellSplit(t *testing.T) {
	got, err := shellSplit("docker run -d \\\n  -e 'A=b c' --name \"x y\" nginx")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docker", "run", "-d", "-e", "A=b c", "--name", "x y", "nginx"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	if _, err := shellSplit("echo 'oops"); err == nil {
		t.Error("unterminated quote should fail")
	}
}

func TestHistory(t *testing.T) {
	root := t.TempDir()
	f := &Files{cfg: &Config{QuadletDir: root, StateDir: t.TempDir()}}
	if err := f.Create("a.container", "v1"); err != nil {
		t.Fatal(err)
	}
	f.Write("a.container", "v2")
	f.Write("a.container", "v2") // identical content adds no version
	f.Write("a.container", "v3")
	vs, _ := f.History("a.container")
	if len(vs) != 2 {
		t.Fatalf("versions = %d, want 2", len(vs))
	}
	if c, _ := f.ReadVersion("a.container", vs[0].ID); c != "v2" {
		t.Errorf("newest version = %q", c)
	}
	if err := f.Create("a.container", "x"); err == nil {
		t.Error("Create should not overwrite")
	}
}

func TestQuadletMessages(t *testing.T) {
	stderr := `quadlet-generator[1]: Loading source unit file /tmp/x/a.container
quadlet-generator[1]: Loading source unit file /tmp/x/zz.container
quadlet-generator[1]: Warning: a.container specifies the image "x" which not a fully qualified image name.
quadlet-generator[1]: Warning: zz.container specifies the image "x" which not a fully qualified image name.
quadlet-generator[1]: converting "zz.container": unsupported key 'BogusKey' in group 'Container' in /tmp/x/zz.container
`
	out, problems := quadletMessages(stderr, "a.container")
	if len(problems) != 0 || out != `Warning: a.container specifies the image "x" which not a fully qualified image name.` {
		t.Errorf("a: %q %q", out, problems)
	}
	_, problems = quadletMessages(stderr, "stack/zz.container")
	if len(problems) != 1 || problems[0] != `converting "zz.container": unsupported key 'BogusKey' in group 'Container' in /tmp/x/zz.container` {
		t.Errorf("zz: %q", problems)
	}
}

func TestResources(t *testing.T) {
	root := writeTree(t, map[string]string{
		"media.network":          "[Network]\nNetworkName=media\n",
		"media.network.d/x.conf": "[Network]\nInternal=true\n",
		"db.volume":              "[Volume]\n",
		"orphan.network":         "[Network]\n",
		"arr.pod":                "[Pod]\nNetwork=media.network\n",
		"sonarr.container": "[Container]\nImage=x\nPod=arr.pod\nVolume=/DATA/media:/data:ro\nVolume=db.volume:/db\n" +
			"Mount=type=bind,source=/srv,destination=/srv,ro\nSecret=api-key,type=env,target=API_KEY\nSecret=tls\n",
		"kavita.container":  "[Container]\nImage=y\nNetwork=media:ip=10.89.0.5\n",
		"web/web.network":   "[Network]\n",
		"web/app.container": "[Container]\nImage=z\nNetwork=web.network\n",
	})
	inv, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range inv.Stacks {
		if s.ID == "r/media" || s.ID == "r/db" || s.ID == "r/orphan" {
			t.Errorf("shared resource %s should not be a stack", s.ID)
		}
	}
	got := map[string][]string{}
	for _, r := range inv.Resources {
		got[r.Path+"@"+r.Stack] = r.UsedBy
	}
	want := map[string][]string{
		"db.volume@":              {"r/arr"},
		"media.network@":          {"r/arr", "r/kavita"},
		"media.network.d/x.conf@": {},
		"orphan.network@":         {},
		"web/web.network@d/web":   {"d/web"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resources\n got %v\nwant %v", got, want)
	}

	arr, _ := FindStack(root, "r/arr")
	son := arr.Files[1]
	wantMounts := []Mount{
		{Kind: "bind", Source: "/DATA/media", Target: "/data", Options: "ro"},
		{Kind: "quadlet", Source: "db.volume", Target: "/db"},
		{Kind: "bind", Source: "/srv", Target: "/srv", Options: "ro"},
	}
	if !reflect.DeepEqual(son.Mounts, wantMounts) {
		t.Errorf("mounts = %+v", son.Mounts)
	}
	wantSecrets := []SecretRef{{"api-key", "env", "API_KEY"}, {"tls", "mount", "/run/secrets/tls"}}
	if !reflect.DeepEqual(son.Secrets, wantSecrets) {
		t.Errorf("secrets = %+v", son.Secrets)
	}
}

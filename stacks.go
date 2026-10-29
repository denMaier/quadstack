package main

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/coreos/go-systemd/v22/unit"
)

// Quadlet file kinds: the extension, its main section and the suffix of the generated service
var kinds = map[string]struct{ section, suffix string }{
	".container": {"Container", ""},
	".pod":       {"Pod", "-pod"},
	".network":   {"Network", "-network"},
	".volume":    {"Volume", "-volume"},
	".kube":      {"Kube", ""},
	".image":     {"Image", "-image"},
	".build":     {"Build", "-build"},
	".artifact":  {"Artifact", "-artifact"},
}

// Other files shown and editable next to the units: drop-ins and env files
var extraExts = []string{".conf", ".env"}

// QFile is one file below the Quadlet directory
type QFile struct {
	Path          string `json:"path"` // relative to the Quadlet directory, slash separated
	Kind          string `json:"kind"` // container, pod, …, dropin, env
	Unit          string `json:"unit,omitempty"`
	Pod           string `json:"pod,omitempty"` // Pod= of a container
	Image         string `json:"image,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
	Autostart     bool   `json:"autostart"`
	ParseError    string `json:"parseError,omitempty"`
}

// Stack groups files like a compose project: a subdirectory, or a pod and its containers at the top level
type Stack struct {
	ID    string   `json:"id"` // "d/<dir>" or "r/<name>"
	Name  string   `json:"name"`
	Dir   string   `json:"dir"` // where new files of this stack go
	Files []*QFile `json:"files"`
}

func (s *Stack) Units() []string {
	var out []string
	for _, f := range s.Files {
		// networks, volumes, images and builds are pulled in by the units that need them
		if f.Kind == "container" || f.Kind == "pod" || f.Kind == "kube" {
			out = append(out, f.Unit)
		}
	}
	return out
}

func isManaged(name string) bool {
	ext := filepath.Ext(name)
	_, ok := kinds[ext]
	for _, e := range extraExts {
		ok = ok || ext == e
	}
	return ok && !strings.HasPrefix(name, ".")
}

// cleanRel validates a client supplied path and keeps it inside the Quadlet directory
func cleanRel(p string) (string, error) {
	p = path.Clean("/" + strings.ReplaceAll(p, "\\", "/"))[1:]
	if p == "" || p == "." {
		return "", errors.New("empty path")
	}
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, ".") {
			return "", errors.New("hidden paths are not allowed")
		}
	}
	return p, nil
}

func parseFile(abs, rel string) *QFile {
	f := &QFile{Path: rel}
	base := path.Base(rel)
	ext := path.Ext(base)
	k, isUnit := kinds[ext]
	switch {
	case isUnit:
		f.Kind = ext[1:]
	case ext == ".conf":
		f.Kind = "dropin"
		return f
	default:
		f.Kind = "env"
		return f
	}
	stem := strings.TrimSuffix(base, ext)
	f.Unit = stem + k.suffix + ".service"

	fh, err := os.Open(abs)
	if err != nil {
		f.ParseError = err.Error()
		return f
	}
	defer fh.Close()
	opts, err := unit.DeserializeOptions(fh)
	if err != nil {
		f.ParseError = err.Error()
		return f
	}
	for _, o := range opts {
		switch {
		case o.Section == k.section && o.Name == "ServiceName":
			f.Unit = o.Value + ".service"
		case o.Section == "Container" && o.Name == "Pod":
			f.Pod = o.Value
		case o.Section == "Container" && o.Name == "Image":
			f.Image = o.Value
		case o.Section == "Container" && o.Name == "ContainerName":
			f.ContainerName = o.Value
		case o.Section == "Install" && (o.Name == "WantedBy" || o.Name == "RequiredBy"):
			f.Autostart = true
		}
	}
	if f.Kind == "container" && f.ContainerName == "" {
		f.ContainerName = "systemd-" + stem
	}
	return f
}

// ScanStacks reads the Quadlet directory. Subdirectories are stacks; at the top level a
// .pod forms a stack with the containers that join it, and every other unit stands alone.
func ScanStacks(root string) ([]*Stack, error) {
	byID := map[string]*Stack{}
	get := func(id, name, dir string) *Stack {
		if s, ok := byID[id]; ok {
			return s
		}
		s := &Stack{ID: id, Name: name, Dir: dir}
		byID[id] = s
		return s
	}

	var top []*QFile
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if abs == root {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, abs)
		rel = filepath.ToSlash(rel)
		first, _, nested := strings.Cut(rel, "/")
		// A top-level directory that is not a drop-in directory is a stack, even when empty
		if d.IsDir() {
			if !nested && !strings.HasSuffix(first, ".d") {
				get("d/"+first, first, first)
			}
			return nil
		}
		if !d.Type().IsRegular() || !isManaged(d.Name()) {
			return nil
		}
		f := parseFile(abs, rel)
		if nested && !strings.HasSuffix(first, ".d") {
			s := get("d/"+first, first, first)
			s.Files = append(s.Files, f)
		} else {
			top = append(top, f)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Top level: pods first, so their containers (and drop-ins) can join them
	owner := map[string]*Stack{} // file base (e.g. "web.pod", "app.container") -> stack
	for _, f := range top {
		if f.Kind == "pod" {
			stem := strings.TrimSuffix(path.Base(f.Path), ".pod")
			s := get("r/"+stem, stem, "")
			s.Files = append(s.Files, f)
			owner[path.Base(f.Path)] = s
		}
	}
	for _, f := range top {
		if f.Kind == "pod" || f.Kind == "dropin" || f.Kind == "env" {
			continue
		}
		base := path.Base(f.Path)
		s, ok := owner[f.Pod]
		if !ok {
			stem := strings.TrimSuffix(base, path.Ext(base))
			s = get("r/"+stem, stem, "")
		}
		s.Files = append(s.Files, f)
		owner[base] = s
	}
	for _, f := range top {
		if f.Kind != "dropin" && f.Kind != "env" {
			continue
		}
		// web.container.d/10-x.conf belongs with web.container; web.env with web.*
		dir, base := path.Split(f.Path)
		key := strings.TrimSuffix(strings.TrimSuffix(dir, "/"), ".d")
		if key == "" {
			key = strings.TrimSuffix(base, path.Ext(base))
		}
		s := owner[key]
		if s == nil {
			for b, st := range owner {
				if strings.TrimSuffix(b, path.Ext(b)) == key {
					s = st
					break
				}
			}
		}
		if s == nil {
			s = get("r/"+key, key, "")
		}
		s.Files = append(s.Files, f)
	}

	out := make([]*Stack, 0, len(byID))
	for _, s := range byID {
		sort.Slice(s.Files, func(i, j int) bool { return fileOrder(s.Files[i]) < fileOrder(s.Files[j]) })
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

func fileOrder(f *QFile) string {
	rank := map[string]string{"pod": "0", "container": "1", "kube": "1", "network": "2", "volume": "3", "image": "4", "build": "4", "artifact": "4", "dropin": "5", "env": "6"}
	return rank[f.Kind] + f.Path
}

func FindStack(root, id string) (*Stack, error) {
	stacks, err := ScanStacks(root)
	if err != nil {
		return nil, err
	}
	for _, s := range stacks {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, fs.ErrNotExist
}

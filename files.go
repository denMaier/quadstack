package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const historyKeep = 50

// Files reads and writes below the Quadlet directory and keeps a local edit history
type Files struct {
	cfg *Config
}

func (fl *Files) abs(rel string) string {
	return filepath.Join(fl.cfg.QuadletDir, filepath.FromSlash(rel))
}

func (fl *Files) Read(rel string) (string, error) {
	b, err := os.ReadFile(fl.abs(rel))
	return string(b), err
}

// Validation is the generator's verdict on the directory with one file replaced
type Validation struct {
	OK        bool   `json:"ok"`
	Skipped   string `json:"skipped,omitempty"` // why no check ran
	Output    string `json:"output"`            // warnings and errors from quadlet
	Generated string `json:"generated"`         // the systemd unit generated for this file
}

// scratchCopy copies the managed files to a temporary directory in which rel holds content
// (or is gone when content is nil). The caller removes the directory.
func (fl *Files) scratchCopy(rel string, content *string) (string, error) {
	tmp, err := os.MkdirTemp(fl.cfg.StateDir, "scratch-")
	if err != nil {
		return "", err
	}
	err = filepath.WalkDir(fl.cfg.QuadletDir, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(fl.cfg.QuadletDir, abs)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(tmp, r), 0o700)
		}
		if !d.Type().IsRegular() || !isManaged(d.Name()) {
			return nil
		}
		b, err := os.ReadFile(abs)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(tmp, r), b, 0o600)
	})
	target := filepath.Join(tmp, filepath.FromSlash(rel))
	switch {
	case err != nil:
	case content == nil:
		os.Remove(target)
	default:
		os.MkdirAll(filepath.Dir(target), 0o700)
		err = os.WriteFile(target, []byte(*content), 0o600)
	}
	if err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	return tmp, nil
}

// ScanWith is Scan as it would be after rel is written with content (or deleted when content is nil)
func (fl *Files) ScanWith(rel string, content *string) (*Inventory, error) {
	tmp, err := fl.scratchCopy(rel, content)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	return Scan(tmp)
}

// Validate runs `quadlet -dryrun -user` on a scratch copy of the Quadlet directory in which
// rel holds content (or is gone when content is nil), so references between files resolve.
func (fl *Files) Validate(ctx context.Context, rel string, content *string) Validation {
	if fl.cfg.QuadletBin == "" {
		return Validation{OK: true, Skipped: "Podman's quadlet generator was not found; set QUADSTACK_QUADLET_BIN."}
	}
	tmp, err := fl.scratchCopy(rel, content)
	if err != nil {
		return Validation{Output: err.Error()}
	}
	defer os.RemoveAll(tmp)
	target := filepath.Join(tmp, filepath.FromSlash(rel))

	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, fl.cfg.QuadletBin, "-dryrun", "-user", "-no-kmsg-log")
	cmd.Env = append(os.Environ(), "QUADLET_UNIT_DIRS="+tmp)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	v := Validation{}
	var problems []string
	// Name files relative to the Quadlet directory instead of the scratch copy
	v.Output, problems = quadletMessages(strings.ReplaceAll(stderr.String(), tmp+string(filepath.Separator), ""), rel)
	if content != nil {
		if f := parseFile(target, rel); f.Unit != "" {
			v.Generated = generatedUnit(stdout.String(), f.Unit)
		}
	}
	// quadlet exits non-zero when any file fails to convert; only this file's errors block the save
	v.OK = runErr == nil || len(problems) == 0
	if !v.OK {
		v.Output = strings.Join(problems, "\n")
	}
	return v
}

// quadletMessages keeps the generator's messages about rel, without the "quadlet-generator[pid]: "
// prefix and the "Loading source unit file" progress lines, and returns its errors separately
func quadletMessages(stderr, rel string) (string, []string) {
	base := path.Base(rel)
	var mine, problems []string
	for _, line := range strings.Split(stderr, "\n") {
		if _, after, ok := strings.Cut(line, "]: "); ok && strings.HasPrefix(line, "quadlet-generator[") {
			line = after
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Loading source unit file") || !strings.Contains(line, base) {
			continue
		}
		mine = append(mine, line)
		if !strings.HasPrefix(line, "Warning:") {
			problems = append(problems, line)
		}
	}
	return strings.Join(mine, "\n"), problems
}

// generatedUnit picks one unit out of quadlet's dry-run output, which separates units with ---name--- lines
func generatedUnit(out, unitName string) string {
	marker := "---" + unitName + "---"
	i := strings.Index(out, marker)
	if i < 0 {
		return ""
	}
	rest := out[i+len(marker):]
	if j := strings.Index(rest, "\n---"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// Write saves content atomically, keeping the previous version in the history
func (fl *Files) Write(rel, content string) error {
	abs := fl.abs(rel)
	mode := fs.FileMode(0o600)
	if st, err := os.Stat(abs); err == nil {
		mode = st.Mode().Perm()
		if err := fl.snapshot(rel); err != nil {
			return fmt.Errorf("saving history: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".quadstack-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), abs)
}

func (fl *Files) Create(rel, content string) error {
	if _, err := os.Stat(fl.abs(rel)); err == nil {
		return fmt.Errorf("%s already exists", rel)
	}
	return fl.Write(rel, content)
}

func (fl *Files) Delete(rel string) error {
	if err := fl.snapshot(rel); err != nil {
		return fmt.Errorf("saving history: %w", err)
	}
	return os.Remove(fl.abs(rel))
}

func (fl *Files) Move(from, to string) error {
	if _, err := os.Stat(fl.abs(to)); err == nil {
		return fmt.Errorf("%s already exists", to)
	}
	if err := os.MkdirAll(filepath.Dir(fl.abs(to)), 0o755); err != nil {
		return err
	}
	return os.Rename(fl.abs(from), fl.abs(to))
}

// CreateStack makes a stack directory
func (fl *Files) CreateStack(name string) error {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".d") {
		return errors.New("invalid stack name")
	}
	return os.Mkdir(fl.abs(name), 0o755)
}

// RemoveStack deletes an empty stack directory
func (fl *Files) RemoveStack(name string) error {
	return os.Remove(fl.abs(name))
}

/* History */

type Version struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Size int64     `json:"size"`
}

func (fl *Files) historyDir(rel string) string {
	return filepath.Join(fl.cfg.StateDir, "history", filepath.FromSlash(rel))
}

func (fl *Files) snapshot(rel string) error {
	b, err := os.ReadFile(fl.abs(rel))
	if err != nil {
		return err
	}
	dir := fl.historyDir(rel)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Don't record a version that is identical to the newest one
	if vs, _ := fl.History(rel); len(vs) > 0 {
		if last, err := os.ReadFile(filepath.Join(dir, vs[0].ID)); err == nil && bytes.Equal(last, b) {
			return nil
		}
	}
	id := strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.WriteFile(filepath.Join(dir, id), b, 0o600); err != nil {
		return err
	}
	vs, _ := fl.History(rel)
	for _, v := range vs[min(len(vs), historyKeep):] {
		os.Remove(filepath.Join(dir, v.ID))
	}
	return nil
}

// History lists the saved versions of a file, newest first
func (fl *Files) History(rel string) ([]Version, error) {
	entries, err := os.ReadDir(fl.historyDir(rel))
	if errors.Is(err, fs.ErrNotExist) {
		return []Version{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Version{}
	for _, e := range entries {
		ns, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil || e.IsDir() {
			continue
		}
		info, _ := e.Info()
		out = append(out, Version{ID: e.Name(), Time: time.Unix(0, ns), Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (fl *Files) ReadVersion(rel, id string) (string, error) {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return "", errors.New("invalid version")
	}
	b, err := os.ReadFile(filepath.Join(fl.historyDir(rel), id))
	return string(b), err
}

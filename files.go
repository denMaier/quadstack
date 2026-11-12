package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const historyKeep = 50

// Files reads and writes below the Quadlet directory and keeps a local edit history
type Files struct {
	cfg *Config
}

func (fl *Files) abs(rel string) string {
	return filepath.Join(fl.cfg.QuadletDir, filepath.FromSlash(rel))
}

var errLinked = errors.New("symlinked files and folders are not managed by Quadstack")

// parent opens every directory of rel without following links, for the web UI and MCP alike.
// Holding each directory descriptor prevents a concurrent rename from redirecting the operation.
func (fl *Files) parent(rel string, create bool) (int, string, error) {
	parts := strings.Split(rel, "/")
	// The Quadlet directory is the operator's setting, so it may itself be a link
	fd, err := unix.Open(fl.cfg.QuadletDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(e, unix.ENOENT) && create {
			if e = unix.Mkdirat(fd, part, 0o755); e == nil || errors.Is(e, unix.EEXIST) {
				next, e = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			}
		}
		if e != nil {
			e = linkErr(fd, part, e)
		}
		unix.Close(fd)
		if e != nil {
			return -1, "", e
		}
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

// linkErr names the reason when opening name below fd failed because it is a symlink
func linkErr(fd int, name string, err error) error {
	var st unix.Stat_t
	if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return errLinked
	}
	return err
}

func readAt(fd int, name string) ([]byte, fs.FileMode, error) {
	f, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, linkErr(fd, name, err)
	}
	file := os.NewFile(uintptr(f), name)
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !st.Mode().IsRegular() {
		return nil, 0, errors.New("not a regular file")
	}
	b, err := io.ReadAll(file)
	return b, st.Mode().Perm(), err
}

func (fl *Files) Read(rel string) (string, error) {
	fd, name, err := fl.parent(rel, false)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	b, _, err := readAt(fd, name)
	return string(b), err
}

// Write saves content atomically, keeping the previous version in the history
func (fl *Files) Write(rel, content string) error {
	fd, name, err := fl.parent(rel, true)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	old, mode, err := readAt(fd, name)
	if err == nil {
		if err = fl.snapshotContent(rel, old); err != nil {
			return fmt.Errorf("saving history: %w", err)
		}
	} else if errors.Is(err, unix.ENOENT) {
		mode = 0o600
	} else {
		return err
	}
	tmp := ".quadstack-" + randomHex(16)
	t, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(mode))
	if err != nil {
		return err
	}
	defer unix.Unlinkat(fd, tmp, 0)
	f := os.NewFile(uintptr(t), tmp)
	if _, err = f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return unix.Renameat(fd, tmp, fd, name)
}

func (fl *Files) Delete(rel string) error {
	fd, name, err := fl.parent(rel, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	b, _, err := readAt(fd, name)
	if err != nil {
		return err
	}
	if err = fl.snapshotContent(rel, b); err != nil {
		return fmt.Errorf("saving history: %w", err)
	}
	return unix.Unlinkat(fd, name, 0)
}

// Move renames a file and takes its history along, so earlier versions stay with it
func (fl *Files) Move(from, to string) error {
	src, name, err := fl.parent(from, false)
	if err != nil {
		return err
	}
	defer unix.Close(src)
	b, _, err := readAt(src, name)
	if err != nil {
		return err
	}
	dst, toName, err := fl.parent(to, true)
	if err != nil {
		return err
	}
	defer unix.Close(dst)
	var st unix.Stat_t
	if unix.Fstatat(dst, toName, &st, unix.AT_SYMLINK_NOFOLLOW) == nil {
		return fmt.Errorf("%s already exists", to)
	}
	if err := fl.snapshotContent(from, b); err != nil {
		return fmt.Errorf("saving history: %w", err)
	}
	if err := unix.Renameat(src, name, dst, toName); err != nil {
		return err
	}
	return fl.moveHistory(from, to)
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
		if !isManaged(d.Name()) || !(d.Type().IsRegular() || isLinkedFile(abs, d)) {
			return nil
		}
		// Linked files are copied too: quadlet follows them, so references to them must resolve
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
	// Only an ordinary non-zero exit is quadlet's verdict; a timeout, a crash or a binary that
	// cannot run says nothing about the file, so it must not let the save through
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		v.Output = "quadlet did not finish: " + ctx.Err().Error()
		return v
	case runErr != nil && (!errors.As(runErr, &exitErr) || exitErr.ExitCode() < 0):
		v.Output = "running quadlet: " + runErr.Error()
		return v
	}
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

func (fl *Files) Create(rel, content string) error {
	if _, err := os.Lstat(fl.abs(rel)); err == nil {
		return fmt.Errorf("%s already exists", rel)
	}
	return fl.Write(rel, content)
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

func (fl *Files) snapshotContent(rel string, b []byte) error {
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
	fl.trimHistory(rel)
	return nil
}

func (fl *Files) trimHistory(rel string) {
	vs, _ := fl.History(rel)
	for _, v := range vs[min(len(vs), historyKeep):] {
		os.Remove(filepath.Join(fl.historyDir(rel), v.ID))
	}
}

// moveHistory gives the versions of from to to. Version IDs are timestamps, so they merge
// with any history to already had from an earlier file of that name.
func (fl *Files) moveHistory(from, to string) error {
	vs, err := fl.History(from)
	if err != nil || len(vs) == 0 {
		return err
	}
	dir := fl.historyDir(to)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, v := range vs {
		if err := os.Rename(filepath.Join(fl.historyDir(from), v.ID), filepath.Join(dir, v.ID)); err != nil {
			return err
		}
	}
	os.Remove(fl.historyDir(from))
	fl.trimHistory(to)
	return nil
}

// DeletedFile is a file that is gone but has saved versions
type DeletedFile struct {
	Path     string    `json:"path"`
	Versions int       `json:"versions"`
	Time     time.Time `json:"time"` // of the newest version, about when it was deleted
}

// Deleted lists the files that have a history but no longer exist, newest first
func (fl *Files) Deleted() ([]DeletedFile, error) {
	root := filepath.Join(fl.cfg.StateDir, "history")
	out := []DeletedFile{}
	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && abs == root {
			return filepath.SkipAll
		}
		if err != nil || !d.IsDir() || abs == root {
			return err
		}
		r, _ := filepath.Rel(root, abs)
		rel := filepath.ToSlash(r)
		if !isManaged(path.Base(rel)) {
			return nil
		}
		if _, err := os.Lstat(fl.abs(rel)); !errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if vs, _ := fl.History(rel); len(vs) > 0 {
			out = append(out, DeletedFile{Path: rel, Versions: len(vs), Time: vs[0].Time})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, err
}

// Forget removes every saved version of a file, for example of an .env file with old credentials
func (fl *Files) Forget(rel string) error {
	vs, err := fl.History(rel)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if err := os.Remove(filepath.Join(fl.historyDir(rel), v.ID)); err != nil {
			return err
		}
	}
	os.Remove(fl.historyDir(rel))
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

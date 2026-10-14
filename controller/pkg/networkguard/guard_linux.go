// Package networkguard bounds an explicitly owned configuration migration.
// The host service is independent of Kubernetes, SSH, and remote heartbeats.
package networkguard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type File struct {
	Path            string `json:"path"`
	CandidatePath   string `json:"candidatePath"`
	Candidate       []byte `json:"candidate"`
	CandidateSHA256 string `json:"candidateSHA256"`
	Before          []byte `json:"before"`
	Mode            uint32 `json:"mode"`
	UID             int    `json:"uid"`
	GID             int    `json:"gid"`
}
type State struct {
	Token           string  `json:"token"`
	BootID          string  `json:"bootID"`
	Deadline        float64 `json:"deadlineUptimeSeconds"`
	Apply           string  `json:"apply"`
	ApplySHA256     string  `json:"applySHA256"`
	ApplyFailures   int     `json:"applyFailures"`
	Restore         string  `json:"restore"`
	RestoreSHA256   string  `json:"restoreSHA256"`
	Files           []File  `json:"files"`
	Outcome         string  `json:"outcome"`
	RestoreAttempts int     `json:"restoreAttempts"`
	RestoreFailures int     `json:"restoreFailures"`
	LastError       string  `json:"lastError,omitempty"`
}
type Commit struct {
	Token  string `json:"token"`
	BootID string `json:"bootID"`
}

func SHA(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func clock() (string, float64, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", 0, err
	}
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "", 0, err
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return "", 0, errors.New("missing boot clock")
	}
	uptime, err := strconv.ParseFloat(fields[0], 64)
	return strings.TrimSpace(string(boot)), uptime, err
}

// Read only regular files reached without symlinks; snapshots are bounded.
func read(path string) ([]byte, os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, nil, fmt.Errorf("noncanonical path %q", path)
	}
	for p := path; p != "/"; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, fmt.Errorf("symlink: %s", p)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 || info.Mode().Perm()&0022 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, nil, fmt.Errorf("file must be regular, bounded, and not group/world writable: %s", path)
	}
	if n, err := unix.Listxattr(path, nil); (err != nil && err != syscall.ENOTSUP) || n > 0 {
		return nil, nil, fmt.Errorf("extended attributes unsupported for guarded file %s: %v", path, err)
	}
	raw, err := os.ReadFile(path)
	return raw, info, err
}
func atomicWrite(path string, data []byte, mode os.FileMode, uid, gid int) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".network-guard-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if err = f.Chown(uid, gid); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func save(dir string, s *State) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if len(raw) > 4<<20 {
		return errors.New("snapshot state exceeds four MiB")
	}
	return atomicWrite(filepath.Join(dir, "state.json"), append(raw, '\n'), 0600, os.Getuid(), os.Getgid())
}
func Load(dir string) (*State, error) {
	raw, _, err := read(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	var s State
	err = json.Unmarshal(raw, &s)
	return &s, err
}
func directory(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return errors.New("state directory must be an absolute canonical path")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		return errors.New("state directory must be private and owned by the guard user")
	}
	for p := filepath.Dir(dir); p != "/"; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("state directory has symlink ancestor")
		}
	}
	return nil
}

// Prepare creates a fresh private transaction. Call before starting the service;
// Watch exclusively installs and applies the captured candidate.
func Prepare(dir, apply, restore string, files []File, within time.Duration) (*State, error) {
	if within <= 0 || within > 4*time.Minute || len(files) == 0 {
		return nil, errors.New("require files and a deadline of at most four minutes (leaving time for restoration)")
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, errors.New("state directory must be absolute and canonical")
	}
	command, info, err := read(restore)
	if err != nil {
		return nil, err
	}
	if int(info.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		return nil, errors.New("restore command must belong to the guard user")
	}
	if info.Mode()&0111 == 0 {
		return nil, errors.New("restore command is not executable")
	}
	applyCommand, applyInfo, err := read(apply)
	if err != nil {
		return nil, err
	}
	if int(applyInfo.Sys().(*syscall.Stat_t).Uid) != os.Getuid() || applyInfo.Mode()&0111 == 0 {
		return nil, errors.New("apply command must be executable and owned by the guard user")
	}
	s := &State{Apply: apply, ApplySHA256: SHA(applyCommand), Restore: restore, RestoreSHA256: SHA(command), Outcome: "prepared"}
	seen := map[string]bool{}
	for _, f := range files {
		digest, err := hex.DecodeString(f.CandidateSHA256)
		if err != nil || len(digest) != 32 || strings.ToLower(f.CandidateSHA256) != f.CandidateSHA256 {
			return nil, errors.New("invalid candidate digest")
		}
		if seen[f.Path] || f.Path == restore || f.Path == apply || strings.HasPrefix(f.Path, dir+"/") {
			return nil, errors.New("duplicate or transaction-owned configuration path")
		}
		seen[f.Path] = true
		raw, info, err := read(f.Path)
		if err != nil {
			return nil, err
		}
		st := info.Sys().(*syscall.Stat_t)
		if int(st.Uid) != os.Getuid() {
			return nil, errors.New("configuration file must belong to the guard user")
		}
		candidate, candidateInfo, err := read(f.CandidatePath)
		if err != nil {
			return nil, err
		}
		if int(candidateInfo.Sys().(*syscall.Stat_t).Uid) != os.Getuid() || SHA(candidate) != f.CandidateSHA256 {
			return nil, errors.New("candidate ownership or digest mismatch")
		}
		f.Candidate = candidate
		f.Before = raw
		f.Mode = uint32(info.Mode().Perm())
		f.UID = int(st.Uid)
		f.GID = int(st.Gid)
		s.Files = append(s.Files, f)
	}
	boot, now, err := clock()
	if err != nil {
		return nil, err
	}
	s.BootID = boot
	s.Deadline = now + within.Seconds()
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return nil, err
	}
	s.Token = hex.EncodeToString(token)
	if err = os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	if err = directory(dir); err != nil {
		return nil, err
	}
	if err = save(dir, s); err != nil {
		return nil, err
	}
	return s, nil
}
func expired(s *State, boot string, now float64) bool { return boot != s.BootID || now >= s.Deadline }
func checkFiles(s *State, candidateOnly bool) error {
	for _, f := range s.Files {
		raw, info, err := read(f.Path)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		if uint32(info.Mode().Perm()) != f.Mode || int(st.Uid) != f.UID || int(st.Gid) != f.GID {
			return fmt.Errorf("configuration metadata changed: %s", f.Path)
		}
		if SHA(raw) != f.CandidateSHA256 && (candidateOnly || SHA(raw) != SHA(f.Before)) {
			return fmt.Errorf("configuration changed outside transaction: %s", f.Path)
		}
	}
	return nil
}

// Approve records a request only while the candidate is intact and time remains.
// The caller must observe committed state before considering the guard disarmed.
func Approve(dir, token string) error {
	if err := directory(dir); err != nil {
		return err
	}
	s, err := Load(dir)
	if err != nil {
		return err
	}
	boot, now, err := clock()
	if err != nil {
		return err
	}
	if token != s.Token || s.Outcome != "armed" || expired(s, boot, now) {
		return errors.New("approval is stale, late, or guard is not armed")
	}
	if err = checkFiles(s, true); err != nil {
		return err
	}
	raw, _ := json.Marshal(Commit{Token: token, BootID: boot})
	return atomicWrite(filepath.Join(dir, "commit.json"), raw, 0600, os.Getuid(), os.Getgid())
}
func restore(s *State) error {
	command, _, err := read(s.Restore)
	if err != nil {
		return err
	}
	if SHA(command) != s.RestoreSHA256 {
		return errors.New("restore command changed")
	}
	if err = checkFiles(s, false); err != nil {
		return err
	}
	for _, f := range s.Files {
		if err = atomicWrite(f.Path, f.Before, os.FileMode(f.Mode), f.UID, f.GID); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := runCommand(ctx, s.Restore)
	if err != nil {
		return fmt.Errorf("restore command failed: %w: %s", err, output)
	}
	return nil
}

// Watch must run as an independent host service with Restart=on-failure. Its
// deadline survives service restart; a host reboot causes immediate rollback.
// File ownership must remain exclusive until commit or completed restoration.
func Watch(ctx context.Context, dir string) error {
	if err := directory(dir); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "watch.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	s, err := Load(dir)
	if err != nil {
		return err
	}
	if s.Outcome == "committed" || s.Outcome == "rolled-back" {
		return nil
	}
	boot, now, err := clock()
	if err != nil {
		return err
	}
	if s.Outcome == "applying" {
		s.ApplyFailures++
		s.LastError = "application interrupted; refusing to replay"
		return attemptRestore(dir, s)
	}
	if expired(s, boot, now) || s.Outcome == "restoring" || s.Outcome == "restore-failed" {
		return attemptRestore(dir, s)
	}
	command, _, err := read(s.Restore)
	if err != nil {
		return err
	}
	if SHA(command) != s.RestoreSHA256 {
		return errors.New("restore command changed before arming")
	}
	if err := checkFiles(s, s.Outcome == "armed"); err != nil {
		return err
	}
	if s.Outcome == "prepared" {
		command, _, err := read(s.Apply)
		if err != nil {
			return err
		}
		if SHA(command) != s.ApplySHA256 {
			return errors.New("apply command changed")
		}
		for _, f := range s.Files {
			raw, _, err := read(f.Path)
			if err != nil {
				return err
			}
			if SHA(raw) != SHA(f.Before) || SHA(f.Candidate) != f.CandidateSHA256 {
				return errors.New("original or captured candidate changed before application")
			}
		}
		s.Outcome = "applying"
		if err = save(dir, s); err != nil {
			return err
		}
		fail := func(cause error) error {
			s.ApplyFailures++
			s.LastError = cause.Error()
			return errors.Join(cause, attemptRestore(dir, s))
		}
		for _, f := range s.Files {
			if err = atomicWrite(f.Path, f.Candidate, os.FileMode(f.Mode), f.UID, f.GID); err != nil {
				return fail(err)
			}
		}
		boot, now, err = clock()
		if err != nil {
			return fail(err)
		}
		if expired(s, boot, now) {
			return fail(errors.New("deadline expired before application"))
		}
		applyCtx, cancel := context.WithTimeout(ctx, time.Duration((s.Deadline-now)*float64(time.Second)))
		output, applyErr := runCommand(applyCtx, s.Apply)
		cancel()
		if applyErr != nil {
			return fail(fmt.Errorf("apply command failed: %w: %s", applyErr, output))
		}
		if err = checkFiles(s, true); err != nil {
			return fail(err)
		}
	} else if s.Outcome != "armed" {
		return fmt.Errorf("unsupported transaction outcome %q", s.Outcome)
	}
	boot, now, err = clock()
	if err != nil {
		return err
	}
	if expired(s, boot, now) {
		return attemptRestore(dir, s)
	}
	s.Outcome = "armed"
	if err = save(dir, s); err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(dir, "ready.json"), []byte("{\"armed\":true}\n"), 0600, os.Getuid(), os.Getgid()); err != nil {
		return err
	}
	for {
		boot, now, err := clock()
		if err != nil {
			return err
		}
		if expired(s, boot, now) {
			return attemptRestore(dir, s)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "commit.json"))
		if err == nil {
			var c Commit
			if json.Unmarshal(raw, &c) == nil && c.Token == s.Token && c.BootID == s.BootID {
				if err = checkFiles(s, true); err != nil {
					return err
				}
				// Recheck after filesystem observations: approval cannot extend the deadline.
				boot, now, err = clock()
				if err != nil {
					return err
				}
				if !expired(s, boot, now) {
					s.Outcome = "committed"
					return save(dir, s)
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Kill the entire command process group on timeout, including script children.
func runCommand(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	// Commands must not leave asynchronous work after reporting success.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return output.data, err
}

type boundedOutput struct{ data []byte }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 8192 - len(b.data)
	if remaining > 0 {
		b.data = append(b.data, p[:min(len(p), remaining)]...)
	}
	return n, nil
}

func attemptRestore(dir string, s *State) error {
	s.RestoreAttempts++
	s.Outcome = "restoring"
	if err := save(dir, s); err != nil {
		return err
	}
	if err := restore(s); err != nil {
		s.RestoreFailures++
		s.LastError = err.Error()
		s.Outcome = "restore-failed"
		return errors.Join(err, save(dir, s))
	}
	s.Outcome = "rolled-back"
	return save(dir, s)
}

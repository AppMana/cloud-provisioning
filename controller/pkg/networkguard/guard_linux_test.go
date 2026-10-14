package networkguard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, within time.Duration) (string, *State, string) {
	t.Helper()
	root := t.TempDir()
	config := filepath.Join(root, "owned.conf")
	restore := filepath.Join(root, "restore")
	if err := os.WriteFile(config, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(restore, []byte("#!/bin/sh\nprintf restored > \""+filepath.Join(root, "receipt")+"\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	apply := filepath.Join(root, "apply")
	os.WriteFile(apply, []byte("#!/bin/sh\nexit 0\n"), 0700)
	candidate := filepath.Join(root, "candidate")
	os.WriteFile(candidate, []byte("candidate"), 0600)
	dir := filepath.Join(root, "state")
	s, err := Prepare(dir, apply, restore, []File{{Path: config, CandidatePath: candidate, CandidateSHA256: SHA([]byte("candidate"))}}, within)
	if err != nil {
		t.Fatal(err)
	}
	return dir, s, config
}
func start(t *testing.T, dir string) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, dir) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "ready.json")); err == nil {
			return cancel, done
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("guard did not arm")
	return nil, nil
}
func TestExpiryRestoresWithoutExternalAcknowledgement(t *testing.T) {
	dir, _, config := fixture(t, 4*time.Second)
	_, done := start(t, dir)
	os.WriteFile(config, []byte("candidate"), 0600)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(config)
	s, _ := Load(dir)
	if string(raw) != "before" || s.Outcome != "rolled-back" {
		t.Fatalf("config=%s state=%+v", raw, s)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "receipt")); err != nil {
		t.Fatal(err)
	}
}
func TestApprovalNeedsExactCandidateAndCurrentToken(t *testing.T) {
	dir, s, config := fixture(t, 3*time.Second)
	if err := Approve(dir, s.Token); err == nil {
		t.Fatal("approved unapplied configuration")
	}
	_, done := start(t, dir)
	if err := Approve(dir, "stale"); err == nil {
		t.Fatal("accepted stale token")
	}
	if err := Approve(dir, s.Token); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(config)
	s, _ = Load(dir)
	if string(raw) != "candidate" || s.Outcome != "committed" {
		t.Fatal(s)
	}
}
func TestRestartDoesNotResetDeadline(t *testing.T) {
	dir, s, config := fixture(t, 4*time.Second)
	cancel, done := start(t, dir)
	os.WriteFile(config, []byte("candidate"), 0600)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("stopped guard returned success")
	}
	if err := Watch(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	final, _ := Load(dir)
	if final.Deadline != s.Deadline || final.Outcome != "rolled-back" {
		t.Fatal(final)
	}
}
func TestRebootAndExpiredPreparationRollbackImmediately(t *testing.T) {
	for _, kind := range []string{"reboot", "expiry"} {
		t.Run(kind, func(t *testing.T) {
			dir, s, config := fixture(t, time.Minute)
			os.WriteFile(config, []byte("candidate"), 0600)
			if kind == "reboot" {
				s.BootID = "previous-boot"
			} else {
				s.Deadline = 0
			}
			save(dir, s)
			if err := Watch(context.Background(), dir); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "ready.json")); !os.IsNotExist(err) {
				t.Fatal("expired guard advertised readiness")
			}
			if err := Approve(dir, s.Token); err == nil {
				t.Fatal("late approval accepted")
			}
		})
	}
}
func TestRestoreRejectsDriftBeforeOverwritingFiles(t *testing.T) {
	for _, kind := range []string{"file", "command", "mode", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			dir, s, config := fixture(t, time.Minute)
			os.WriteFile(config, []byte("candidate"), 0600)
			s.Deadline = 0
			save(dir, s)
			switch kind {
			case "file":
				os.WriteFile(config, []byte("external-edit"), 0600)
			case "command":
				os.WriteFile(s.Restore, []byte("#!/bin/sh\nexit 0\n"), 0700)
			case "mode":
				os.Chmod(config, 0640)
			case "symlink":
				os.Remove(config)
				os.Symlink(s.Restore, config)
			}
			if err := Watch(context.Background(), dir); err == nil {
				t.Fatal("accepted drift")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "receipt")); !os.IsNotExist(err) {
				t.Fatal("ran restore despite drift")
			}
		})
	}
}
func TestFailedRestoreCanRetryWithoutLosingSnapshot(t *testing.T) {
	dir, s, config := fixture(t, time.Minute)
	script := "#!/bin/sh\nif test ! -f \"" + filepath.Join(filepath.Dir(dir), "retry") + "\"; then touch \"" + filepath.Join(filepath.Dir(dir), "retry") + "\"; exit 1; fi\n"
	os.WriteFile(s.Restore, []byte(script), 0700)
	s.RestoreSHA256 = SHA([]byte(script))
	s.Deadline = 0
	save(dir, s)
	os.WriteFile(config, []byte("candidate"), 0600)
	if err := Watch(context.Background(), dir); err == nil {
		t.Fatal("failed restore reported success")
	}
	if err := Watch(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	final, _ := Load(dir)
	if final.Outcome != "rolled-back" || final.RestoreAttempts != 2 || final.RestoreFailures != 1 {
		t.Fatal(final)
	}
}
func TestPrepareRejectsReuseAndInvalidPlan(t *testing.T) {
	dir, s, config := fixture(t, time.Minute)
	for _, files := range [][]File{nil, {{Path: config, CandidateSHA256: "bad"}}, {{Path: config, CandidateSHA256: strings.Repeat("a", 64)}, {Path: config, CandidateSHA256: strings.Repeat("a", 64)}}} {
		if _, err := Prepare(filepath.Join(filepath.Dir(dir), "invalid"), s.Apply, s.Restore, files, time.Minute); err == nil {
			t.Fatal("accepted invalid plan")
		}
	}
	if _, err := Prepare(dir, s.Apply, s.Restore, s.Files, time.Minute); err == nil {
		t.Fatal("reused transaction")
	}
	if _, err := Prepare(filepath.Join(filepath.Dir(dir), "long"), s.Apply, s.Restore, s.Files, 5*time.Minute); err == nil {
		t.Fatal("accepted excessive deadline")
	}
}

func TestRestoreCommandTimeoutDoesNotLeaveBackgroundMutation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "restore")
	marker := filepath.Join(root, "unexpected")
	script := "#!/bin/sh\n(sleep 0.2; touch '" + marker + "') &\nwait\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := runCommand(ctx, path); err == nil {
		t.Fatal("timed-out command succeeded")
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("timed-out child mutated configuration")
	}
}

func TestGuardCannotArmWithInvalidRestoration(t *testing.T) {
	dir, s, _ := fixture(t, time.Minute)
	if err := os.WriteFile(s.Restore, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Watch(context.Background(), dir); err == nil {
		t.Fatal("armed with a changed restore command")
	}
	if _, err := os.Stat(filepath.Join(dir, "ready.json")); !os.IsNotExist(err) {
		t.Fatal("advertised unusable rollback")
	}
}

func TestOwnedApplyFailureCannotBecomeApproved(t *testing.T) {
	for _, kind := range []string{"failure", "timeout", "interrupted", "command-drift", "snapshot-drift"} {
		t.Run(kind, func(t *testing.T) {
			dir, s, config := fixture(t, 2*time.Second)
			switch kind {
			case "failure":
				script := []byte("#!/bin/sh\nexit 9\n")
				os.WriteFile(s.Apply, script, 0700)
				s.ApplySHA256 = SHA(script)
			case "timeout":
				script := []byte("#!/bin/sh\n(sleep 3; printf candidate > '" + config + "') &\nwait\n")
				os.WriteFile(s.Apply, script, 0700)
				s.ApplySHA256 = SHA(script)
			case "interrupted":
				s.Outcome = "applying"
				os.WriteFile(config, []byte("candidate"), 0600)
			case "command-drift":
				os.WriteFile(s.Apply, []byte("#!/bin/sh\nexit 9\n"), 0700)
			case "snapshot-drift":
				s.Files[0].Candidate = []byte("unexpected")
			}
			if err := save(dir, s); err != nil {
				t.Fatal(err)
			}
			err := Watch(context.Background(), dir)
			if kind != "interrupted" && err == nil {
				t.Fatal("failure returned success")
			}
			if kind == "timeout" {
				time.Sleep(3200 * time.Millisecond)
			}
			raw, _ := os.ReadFile(config)
			if string(raw) != "before" {
				t.Fatalf("late candidate write: %s", raw)
			}
			final, _ := Load(dir)
			if kind == "failure" || kind == "timeout" || kind == "interrupted" {
				if final.Outcome != "rolled-back" || final.ApplyFailures != 1 || final.RestoreFailures != 0 {
					t.Fatal(final)
				}
				if err := Watch(context.Background(), dir); err != nil {
					t.Fatal(err)
				}
				final, _ = Load(dir)
				if final.ApplyFailures != 1 {
					t.Fatal("failure evidence lost")
				}
			}
			if err := Approve(dir, s.Token); err == nil {
				t.Fatal("approved failed application")
			}
			if _, err := os.Stat(filepath.Join(dir, "ready.json")); !os.IsNotExist(err) {
				t.Fatal("failed application advertised readiness")
			}
		})
	}
}

func TestCapturedCandidateSurvivesSourceChanges(t *testing.T) {
	dir, s, config := fixture(t, 5*time.Second)
	os.WriteFile(s.Files[0].CandidatePath, []byte("changed-source"), 0600)
	_, done := start(t, dir)
	raw, _ := os.ReadFile(config)
	if string(raw) != "candidate" {
		t.Fatalf("uncaptured source applied: %s", raw)
	}
	if err := Approve(dir, s.Token); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

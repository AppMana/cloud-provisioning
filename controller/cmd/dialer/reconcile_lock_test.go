package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReconciliationLockProcess(t *testing.T) {
	if dir := os.Getenv("CLDT_LOCK_TEST_CHILD"); dir != "" {
		lock, err := lockReconciliation(config{iface: "locktest", peersFile: filepath.Join(dir, "peers.json")})
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReconciliationLockProcess$")
	cmd.Env = append(os.Environ(), "CLDT_LOCK_TEST_CHILD="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "locked" {
		t.Fatalf("child did not acquire lock: %s, %v", scanner.Text(), scanner.Err())
	}
	// Different directory spellings model host/container paths to one mount.
	alias := filepath.Join(t.TempDir(), "shared")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	cfg := config{iface: "locktest", peersFile: filepath.Join(alias, "peers.json")}
	// Neither entry point may read missing inputs or touch native state while a
	// competing process owns the lock. Nil clients make accidental entry visible.
	for _, apply := range []func() error{
		func() error { return reconcile(ctx, nil, nil, cfg) },
		func() error { return reconcileNode(ctx, nil, nil, cfg, nil) },
	} {
		if err := apply(); !errors.Is(err, unix.EWOULDBLOCK) {
			t.Fatalf("reconciliation entered despite competing owner: %v", err)
		}
	}
	independent, err := lockReconciliation(config{iface: "another", peersFile: cfg.peersFile})
	if err != nil {
		t.Fatal(err)
	}
	independent.Close()
	// An abrupt owner exit must release the lock without removing its inode.
	path := filepath.Join(dir, "locktest.reconcile.lock")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waited = true
	for range 2 {
		lock, err := lockReconciliation(cfg)
		if err != nil {
			t.Fatal(err)
		}
		current, err := lock.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, current) {
			t.Fatal("lock inode changed across owners")
		}
		lock.Close()
	}
}

func TestReconciliationLockRejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	cfg := config{iface: "locktest", privateKeyFile: filepath.Join(dir, "private.key")}
	path := filepath.Join(dir, "locktest.reconcile.lock")
	target := filepath.Join(dir, "other")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if lock, err := lockReconciliation(cfg); err == nil {
		lock.Close()
		t.Fatal("accepted symlink lock")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if lock, err := lockReconciliation(cfg); err == nil {
		lock.Close()
		t.Fatal("accepted FIFO lock")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "unchanged" {
		t.Fatal("lock attempt changed target")
	}
	for _, iface := range []string{"", "../escape", ".", ".."} {
		cfg.iface = iface
		if lock, err := lockReconciliation(cfg); err == nil {
			lock.Close()
			t.Fatal("accepted invalid interface")
		}
	}
}

func TestLegacyReconciliationPreservesHandoverRecovery(t *testing.T) {
	for _, kind := range []string{"corrupt", "directory", "dangling-symlink", "unknown-version"} {
		t.Run(kind, func(t *testing.T) {
			cfg := config{iface: "locktest", peersFile: filepath.Join(t.TempDir(), "peers.json")}
			path := filepath.Join(filepath.Dir(cfg.peersFile), cfg.iface+".reconcile.lock.receive-transition-test.json")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0700)
			case "dangling-symlink":
				err = os.Symlink("missing", path)
			case "unknown-version":
				err = os.WriteFile(path, []byte(`{"version":999}`), 0600)
			default:
				err = os.WriteFile(path, []byte("incomplete"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				for _, apply := range []func() error{
					func() error { return reconcile(context.Background(), nil, nil, cfg) },
					func() error { return reconcileNode(context.Background(), nil, nil, cfg, nil) },
					func() error {
						lock, err := lockLegacyReconciliation(cfg)
						if lock != nil {
							lock.Close()
						}
						return err
					},
				} {
					if err := apply(); !errors.Is(err, errReceivePhaseRequired) {
						t.Fatalf("legacy writer bypassed recovery: %v", err)
					}
				}
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) {
				t.Fatal("legacy writer changed recovery state", err)
			}
			// The guard must release the lock so phase-aware recovery can proceed.
			lock, err := lockReconciliation(cfg)
			if err != nil {
				t.Fatal(err)
			}
			lock.Close()
			// A different tunnel on the same host has an independent lifetime.
			cfg.iface = "other"
			lock, err = lockLegacyReconciliation(cfg)
			if err != nil {
				t.Fatal(err)
			}
			lock.Close()
		})
	}
}

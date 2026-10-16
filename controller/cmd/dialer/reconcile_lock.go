package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The lock lives beside identity/recovery files on the existing shared host
// mount, so bootstrap and pod processes serialize reads as well as mutations.
// Keep the inode permanently: unlinking a released lock permits two owners.
// This is local serialization, not journal authorization or a distributed lease.
func lockReconciliation(cfg config) (*os.File, error) {
	if cfg.iface == "" || len(cfg.iface) > 15 || strings.ContainsAny(cfg.iface, "/\\\x00") || cfg.iface == "." || cfg.iface == ".." {
		return nil, fmt.Errorf("reconciliation lock requires a valid interface name")
	}
	identity := cfg.privateKeyFile
	if cfg.peersFile != "" {
		identity = cfg.peersFile
	}
	if identity == "" {
		return nil, fmt.Errorf("reconciliation lock requires shared identity storage")
	}
	dir := filepath.Dir(identity)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, cfg.iface+".reconcile.lock")
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening reconciliation lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	fail := func(err error) (*os.File, error) { file.Close(); return nil, err }
	info, err := file.Stat()
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("reconciliation lock is not a regular file"))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fail(fmt.Errorf("acquiring reconciliation lock for %s: %w", cfg.iface, err))
	}
	current, err := os.Lstat(path)
	if err != nil {
		return fail(err)
	}
	if !os.SameFile(info, current) {
		return fail(fmt.Errorf("reconciliation lock was replaced"))
	}
	return file, nil // Close releases the kernel lock, including on process exit.
}

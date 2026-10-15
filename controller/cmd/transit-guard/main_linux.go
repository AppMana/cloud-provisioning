// transit-guard manages a journaled fallback for a transit-only agent upgrade.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/appmana/cloud-provisioning/controller/pkg/transitguard"
)

func run() error {
	path := flag.String("plan", "", "private plan file; prepare creates it exclusively")
	iface := flag.String("interface", "", "owned tunnel interface; it must be absent")
	source := flag.Int("source-table", 0, "owned transit table and rule priority")
	shadow := flag.Int("shadow-table", 0, "unused temporary routing table")
	flag.Parse()
	if os.Geteuid() != 0 {
		return fmt.Errorf("run as root inside the intended node network namespace")
	}
	if flag.NArg() != 1 || *path == "" {
		return fmt.Errorf("require -plan and prepare, install, verify, or remove")
	}
	action := flag.Arg(0)
	switch action {
	case "prepare", "install", "verify", "remove":
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if action == "prepare" {
		p, err := transitguard.Observe(transitguard.Config{Interface: *iface, SourceTable: *source, ShadowTable: *shadow})
		if err != nil {
			return err
		}
		f, err := os.OpenFile(*path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := json.NewEncoder(f).Encode(p); err != nil {
			return err
		}
		return f.Sync()
	}
	f, err := os.OpenFile(*path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4<<20 {
		return fmt.Errorf("plan must be a bounded, private, root-owned regular file")
	}
	d := json.NewDecoder(io.LimitReader(f, 4<<20))
	d.DisallowUnknownFields()
	var p transitguard.Plan
	if err := d.Decode(&p); err != nil {
		return err
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("plan contains trailing data")
	}
	switch action {
	case "install":
		return transitguard.Install(p)
	case "verify":
		return transitguard.Verify(p)
	case "remove":
		return transitguard.Remove(p)
	}
	return fmt.Errorf("unhandled action")
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

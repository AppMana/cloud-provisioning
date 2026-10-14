// network-guard is a node-local migration watchdog, independent of cluster APIs.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/networkguard"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	dir := flag.String("state", "", "fresh absolute private state directory")
	plan := flag.String("plan", "", "JSON plan: apply/restore executables and files with candidatePath/candidateSHA256")
	token := flag.String("token", "", "transaction token for approval")
	within := flag.Duration("within", 4*time.Minute, "fixed rollback deadline, at most four minutes")
	flag.Parse()
	if os.Geteuid() != 0 {
		return fmt.Errorf("network-guard must run as root")
	}
	if flag.NArg() != 1 {
		return fmt.Errorf("expected prepare, watch, approve, or status")
	}
	switch flag.Arg(0) {
	case "prepare":
		raw, err := os.ReadFile(*plan)
		if err != nil {
			return err
		}
		var p struct {
			Apply   string              `json:"apply"`
			Restore string              `json:"restore"`
			Files   []networkguard.File `json:"files"`
		}
		if err = json.Unmarshal(raw, &p); err != nil {
			return err
		}
		s, err := networkguard.Prepare(*dir, p.Apply, p.Restore, p.Files, *within)
		if err != nil {
			return err
		}
		return printState(s)
	case "watch":
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer cancel()
		return networkguard.Watch(ctx, *dir)
	case "approve":
		if err := networkguard.Approve(*dir, *token); err != nil {
			return err
		}
		for range 30 {
			s, err := networkguard.Load(*dir)
			if err != nil {
				return err
			}
			if s.Outcome == "committed" {
				return nil
			}
			if s.Outcome == "rolled-back" {
				return fmt.Errorf("deadline expired before approval completed")
			}
			time.Sleep(100 * time.Millisecond)
		}
		return fmt.Errorf("approval not acknowledged; guard remains armed")
	case "status":
		s, err := networkguard.Load(*dir)
		if err != nil {
			return err
		}
		return printState(s)
	default:
		return fmt.Errorf("unknown command %q", flag.Arg(0))
	}
}

func printState(s *networkguard.State) error {
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"token": s.Token, "bootID": s.BootID, "deadlineUptimeSeconds": s.Deadline, "outcome": s.Outcome, "applyFailures": s.ApplyFailures, "restoreAttempts": s.RestoreAttempts, "restoreFailures": s.RestoreFailures, "lastError": s.LastError})
}

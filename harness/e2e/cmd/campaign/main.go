// Command campaign runs supported profiles serially and records fresh outcomes.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/network"
)

type Row struct {
	Profile            network.Profile
	Status             string
	Started, Finished  time.Time
	Args               []string
	Log, Events, Error string
	Disk               string
}
type Report struct {
	Revision       string
	WorktreeStatus string
	HarnessSHA256  string
	Started        time.Time
	Rows           []Row
}

func main() {
	profiles := flag.String("profiles", "k0s/calico,k0s/kube-router", "supported distro/network rows")
	root := flag.String("work-dir", ".state/campaign", "fresh campaign artifacts")
	disk := flag.String("platform-image", ".state/platform/platform-node.qcow2", "prepared guest-agent platform disk")
	kubeadmDisk := flag.String("kubeadm-image", ".state/kubeadm/kubeadm-node.qcow2", "prepared kubeadm guest disk")
	placements := flag.String("placements", "control-plane,one-worker,two-workers,all-nodes", "placements per profile")
	outage := flag.String("outage", "", "optional victim:cut or victim:reboot")
	keepGoing := flag.Bool("keep-going", false, "continue to later profiles after a failure (replaces the failed lab)")
	within := flag.Duration("timeout", 4*time.Hour, "deadline per profile")
	flag.Parse()
	report := Report{Started: time.Now().UTC()}
	for _, selection := range strings.Split(*profiles, ",") {
		distro, cni, ok := strings.Cut(selection, "/")
		if !ok {
			fatal("profile must be distro/network")
		}
		p, err := network.Select(distro, cni)
		if err != nil {
			fatal(err.Error())
		}
		report.Rows = append(report.Rows, Row{Profile: p, Status: "not-run"})
	}
	for i := range report.Rows {
		row := &report.Rows[i]
		path := *disk
		if row.Profile.Distro == "kubeadm" {
			path = *kubeadmDisk
		}
		image, err := filepath.Abs(path)
		if err != nil {
			fatal(err.Error())
		}
		if _, err := os.Stat(image); err != nil {
			fatal(err.Error())
		}
		row.Disk = image
	}
	work, err := filepath.Abs(*root)
	if err != nil {
		fatal(err.Error())
	}
	if err := os.MkdirAll(work, 0700); err != nil {
		fatal(err.Error())
	}
	// A campaign directory is immutable evidence; refuse to overwrite a previous run.
	reportFile := filepath.Join(work, "report.json")
	f, err := os.OpenFile(reportFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		fatal(err.Error())
	}
	f.Close()
	save := func() {
		raw, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fatal(err.Error())
		}
		if err := os.WriteFile(reportFile, raw, 0600); err != nil {
			fatal(err.Error())
		}
	}
	rev, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		fatal(err.Error())
	}
	report.Revision = strings.TrimSpace(string(rev))
	status, err := exec.Command("git", "status", "--porcelain").Output()
	if err != nil {
		fatal(err.Error())
	}
	report.WorktreeStatus = string(status)
	save()
	binary := filepath.Join(work, "lab")
	build := exec.Command("go", "build", "-cover", "-coverpkg=./...", "-o", binary, "./cmd/lab")
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fatal(err.Error())
	}
	built, err := os.ReadFile(binary)
	if err != nil {
		fatal(err.Error())
	}
	report.HarnessSHA256 = fmt.Sprintf("%x", sha256.Sum256(built))
	save()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	failed := false
	for i := range report.Rows {
		row := &report.Rows[i]
		dir := filepath.Join(work, fmt.Sprintf("%02d-%s-%s", i, row.Profile.Distro, row.Profile.Network))
		if err := os.MkdirAll(dir, 0700); err != nil {
			fatal(err.Error())
		}
		imageName := "platform-node.qcow2"
		if row.Profile.Distro == "kubeadm" {
			imageName = "kubeadm-node.qcow2"
		}
		if err := os.Symlink(row.Disk, filepath.Join(dir, imageName)); err != nil {
			fatal(err.Error())
		}
		row.Log = filepath.Join(dir, "run.log")
		row.Events = filepath.Join(dir, "events.jsonl")
		row.Args = []string{"-rig", "vm", "-distro", row.Profile.Distro, "-cni", row.Profile.Network, "-product", "-remotes", "remote1,remote2", "-check", "-placements", *placements, "-work-dir", dir, "-timeout", within.String()}
		if *outage != "" {
			row.Args = append(row.Args, "-outage", *outage)
		}
		log, err := os.OpenFile(row.Log, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			fatal(err.Error())
		}
		row.Status = "running"
		row.Started = time.Now().UTC()
		save()
		fmt.Printf("%s/%s: %s\n", row.Profile.Distro, row.Profile.Network, row.Log)
		runCtx, stop := context.WithTimeout(ctx, *within+time.Minute)
		coverageDir := filepath.Join(dir, "coverage")
		if err := os.MkdirAll(coverageDir, 0700); err != nil {
			fatal(err.Error())
		}
		cmd := exec.CommandContext(runCtx, binary, row.Args...)
		cmd.Env = append(os.Environ(), "GOCOVERDIR="+coverageDir)
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 4 * time.Minute
		cmd.Stdout = log
		cmd.Stderr = log
		err = cmd.Run()
		stop()
		log.Close()
		row.Finished = time.Now().UTC()
		row.Status = "passed"
		if err != nil {
			row.Status = "failed"
			row.Error = err.Error()
			failed = true
		}
		save()
		if ctx.Err() != nil {
			failed = true
			break
		}
		if err != nil && !*keepGoing {
			break
		}
	}
	fmt.Println(reportFile)
	if failed {
		os.Exit(1)
	}
}
func fatal(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }

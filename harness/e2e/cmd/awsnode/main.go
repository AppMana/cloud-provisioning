// awsnode executes diagnostics on an existing EC2 machine through the AWS rig.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/aws"
)

func main() {
	work := flag.String("work-dir", "", "private AWS run directory")
	instance := flag.String("instance-id", "", "existing EC2 instance ID")
	bootstrapWait := flag.Duration("bootstrap-wait", 0, "wait for completed bootstrap before reporting; requires -bootstrap-status")
	bootstrapStatus := flag.Bool("bootstrap-status", false, "observe native first-boot completion through the guest adapter")
	stdin := flag.Bool("stdin", false, "stage stdin privately for the command")
	put := flag.String("put", "", "write stdin to this protected guest file instead of executing a command")
	timeout := flag.Duration("timeout", 5*time.Minute, "command deadline")
	flag.Parse()
	if *bootstrapWait < 0 || (*bootstrapWait > 0 && !*bootstrapStatus) || *work == "" || *instance == "" || (!*bootstrapStatus && *put == "" && flag.NArg() == 0) || (*put != "" && (flag.NArg() != 0 || *stdin)) || (*bootstrapStatus && (flag.NArg() != 0 || *stdin || *put != "")) {
		fmt.Fprintln(os.Stderr, "-work-dir and -instance-id are required; select a command, -put or -bootstrap-status")
		os.Exit(2)
	}
	raw, err := os.ReadFile(filepath.Join(*work, "resources.json"))
	if err != nil {
		fatal(err)
	}
	var state struct {
		Region, AssetBucket string
		CleanedUp           bool
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		fatal(err)
	}
	if state.CleanedUp {
		fatal(fmt.Errorf("test run has been cleaned up"))
	}
	cli := &aws.CLI{Region: state.Region, SessionPath: filepath.Join(*work, "harness-session.json")}
	node := &aws.Node{API: cli, Input: &aws.S3Input{CLI: cli, Bucket: state.AssetBucket}, NodeName: *instance, InstanceID: *instance}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	node.Commands, err = aws.DiscoverGuestCommands(ctx, cli, *instance)
	if err != nil {
		fatal(err)
	}
	if *bootstrapStatus {
		var complete bool
		if *bootstrapWait > 0 {
			if err := rig.WaitBootstrap(ctx, node, *bootstrapWait); err != nil {
				fatal(err)
			}
			complete = true
		} else {
			complete, err = node.BootstrapComplete(ctx)
			if err != nil {
				fatal(err)
			}
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"complete": complete, "instanceID": *instance}); err != nil {
			fatal(err)
		}
		return
	}
	var out []byte
	if *put != "" {
		err = node.Put(ctx, os.Stdin, *put, 0600)
	} else if *stdin {
		out, err = node.Pipe(ctx, os.Stdin, flag.Args()...)
	} else {
		out, err = node.Exec(ctx, flag.Args()...)
	}
	if err != nil {
		fatal(err)
	}
	if _, err = os.Stdout.Write(out); err != nil {
		fatal(err)
	}
}
func fatal(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }

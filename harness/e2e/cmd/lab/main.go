// Command lab brings the topology up and leaves it standing.
//
// For working on the lab by hand: the rows themselves run under go
// test, where each is a subtest and the results are values rather
// than something to grep. This is the door into the same machinery
// when what is wanted is a lab to look at.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/bringup"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig/container"
)

func main() {
	var (
		workDir = flag.String("work-dir", "_work", "where the generated topology is written")
		down    = flag.Bool("down", false, "destroy the lab instead of building it")
		timeout = flag.Duration("timeout", 20*time.Minute, "deadline for the whole bring-up")
	)
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, *timeout)
	defer cancelTimeout()

	topo := lab.Default()
	r := container.New(topo, *workDir)
	host := bringup.LocalHost{Lab: topo.Name}

	if *down {
		if err := r.Down(ctx); err != nil {
			fail("tearing down: %v", err)
		}
		fmt.Println("the lab is gone")
		return
	}

	step("the segments")
	if err := bringup.PrepareHost(ctx, topo, host); err != nil {
		fail("preparing this host: %v", err)
	}

	step("deploying")
	if err := r.Up(ctx); err != nil {
		fail("deploying: %v", err)
	}

	step("addressing and routing")
	if err := bringup.Configure(ctx, topo, r, host); err != nil {
		fail("configuring: %v", err)
	}

	// Before anything is installed, because a topology that does not
	// isolate makes every result taken on it meaningless.
	step("proving it")
	if err := bringup.Prove(ctx, topo, host); err != nil {
		fail("%v", err)
	}

	fmt.Println("\n  the site reaches both clouds")
	fmt.Println("  the two clouds reach each other across the wan")
	fmt.Println("  neither cloud reaches any address any site node holds")
	fmt.Println("  neither cloud reaches the API server, so a tunnel is the only way in")
	fmt.Println("  the site and both clouds reach the internet")
	fmt.Printf("\nsite     %s.0/24      bastion .2  cp .10  cp2 .13  cp3 .14  w1 .11  w2 .12  router .1\n", lab.SitePrefix)
	fmt.Printf("wan      %s.0/24  router .1  edge-a .2  edge-b .3  this host .254\n", lab.WANPrefix)
	fmt.Printf("cloud A  %s.0/24  remote1 .10\n", lab.CloudAPrefix)
	fmt.Printf("cloud B  %s.0/24    remote2 .10\n", lab.CloudBPrefix)
}

func step(name string) { fmt.Printf("--- %s ---\n", name) }

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}

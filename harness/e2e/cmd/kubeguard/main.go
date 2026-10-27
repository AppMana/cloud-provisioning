// Command kubeguard is kubectl on the lab bastion once a site is built. It
// refuses the writes the harness write guard does not allow (package guard)
// and runs the real kubectl for everything else.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"

	"github.com/appmana/cloud-provisioning/harness/e2e/guard"
)

// kubectl is the distribution's own kubectl, which this replaces on PATH.
const kubectl = "/usr/bin/kubectl"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// Manifests on stdin are what is checked, so they are read first and
	// handed on unchanged.
	if readsStdin(args) {
		raw, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		stdin = bytes.NewReader(raw)
		if err := guard.Kubectl(args, raw); err != nil {
			fmt.Fprintln(stderr, err)
			return 3
		}
	} else if err := guard.Kubectl(args, nil); err != nil {
		fmt.Fprintln(stderr, err)
		return 3
	}
	cmd := exec.Command(kubectl, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func readsStdin(args []string) bool {
	for i, a := range args {
		if a == "--filename=-" || ((a == "-f" || a == "--filename") && i+1 < len(args) && args[i+1] == "-") {
			return true
		}
	}
	return slices.Contains(args, "-f-")
}

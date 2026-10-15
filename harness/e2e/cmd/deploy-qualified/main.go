// deploy-qualified gates application of exact qualified manifests to one site.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/qualification"
)

func main() {
	evidence := flag.String("evidence", "", "qualification evidence JSON")
	candidate := flag.String("candidate", "", "expected full immutable Git commit")
	site := flag.String("site", "", "site-a or site-b")
	kubeconfig := flag.String("kubeconfig", "", "explicit target kubeconfig")
	kubeContext := flag.String("context", "", "explicit target context")
	apply := flag.Bool("apply", false, "apply after qualification, cluster identity checks and server dry run")
	flag.Parse()
	if flag.NArg() != 0 || *evidence == "" || *kubeconfig == "" || *kubeContext == "" {
		fmt.Fprintln(os.Stderr, "-evidence, -candidate, -site, -kubeconfig and -context are required; positional arguments are forbidden")
		os.Exit(2)
	}
	prepared, err := qualification.PrepareDeployment(*evidence, *candidate, *site)
	if err == nil {
		err = prepared.Execute(*kubeconfig, *kubeContext, *apply, func(args []string, stdin io.Reader) ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, "kubectl", args...)
			cmd.Stdin = stdin
			cmd.Stderr = os.Stderr
			return cmd.Output()
		})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *apply {
		fmt.Println("Qualified manifest applied; rollout health and rollback guard checks remain required.")
	} else {
		fmt.Println("Qualification, cluster identity and server dry run passed; nothing applied.")
	}
}

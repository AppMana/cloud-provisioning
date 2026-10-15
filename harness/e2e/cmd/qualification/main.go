// qualification is a read-only release gate. It never applies manifests.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/appmana/cloud-provisioning/harness/e2e/qualification"
)

func main() {
	path := flag.String("evidence", "", "qualification JSON with artifact paths relative to its directory")
	list := flag.Bool("list", false, "list all required qualification rows")
	flag.Parse()
	if *list {
		for _, id := range qualification.RequiredRows() {
			fmt.Println(id)
		}
		return
	}
	if *path == "" {
		fmt.Fprintln(os.Stderr, "-evidence is required")
		os.Exit(2)
	}
	e, failures, err := qualification.VerifyFile(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	result := map[string]any{"candidate": e.Candidate, "deploymentEligible": len(failures) == 0, "failures": failures}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if len(failures) > 0 {
		os.Exit(1)
	}
}

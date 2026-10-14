// qualification is a read-only release gate. It never applies manifests.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	raw, err := os.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var e qualification.Evidence
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		fmt.Fprintln(os.Stderr, "unexpected trailing JSON")
		os.Exit(2)
	}
	failures := qualification.Check(e)
	for _, r := range e.Rows {
		if r.Artifact == "" {
			continue
		}
		if filepath.IsAbs(r.Artifact) || !filepath.IsLocal(r.Artifact) {
			failures = append(failures, r.ID+": artifact must be a local relative path")
			continue
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(*path), r.Artifact))
		if err != nil {
			failures = append(failures, r.ID+": artifact unreadable")
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != r.SHA256 {
			failures = append(failures, r.ID+": artifact digest mismatch")
		}
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

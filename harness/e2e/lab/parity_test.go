package lab

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The generated topology must describe the same lab the bash harness
// deploys.
//
// This is the cheapest parity gate there is, and it guards the
// riskiest moment of the migration: the whole argument for trusting
// the Go harness's early results is that it runs the same lab the
// 13-row campaign ran. If a node or a cable differs, the two are not
// measuring the same network and no row-for-row comparison between
// them means anything.
//
// It goes when the bash harness goes.
func TestTheGeneratedTopologyMatchesTheOneBashDeploys(t *testing.T) {
	legacy := filepath.Join("..", "..", "clab", "topo.clab.yml")
	body, err := os.ReadFile(legacy)
	if err != nil {
		t.Skipf("the bash topology is gone, so there is nothing left to be in parity with: %v", err)
	}

	generated, err := Default().ContainerlabYAML(Container)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := nodeNames(generated), nodeNames(string(body)); !sameSet(got, want) {
		t.Errorf("nodes differ:\n  generated %v\n  bash      %v", got, want)
	}
	if got, want := links(generated), links(string(body)); !sameSet(got, want) {
		t.Errorf("links differ:\n  generated %v\n  bash      %v", got, want)
	}
}

// nodeNames reads the node keys out of a containerlab document.
// Deliberately textual rather than a YAML decode: what is being
// compared is two files that must agree, and a decoder that
// normalises them would hide exactly the differences worth catching.
func nodeNames(doc string) []string {
	_, after, ok := strings.Cut(doc, "\n  nodes:\n")
	if !ok {
		return nil
	}
	if before, _, ok := strings.Cut(after, "\n  links:"); ok {
		after = before
	}
	var out []string
	for _, line := range strings.Split(after, "\n") {
		if m := regexp.MustCompile(`^    ([a-z0-9][a-z0-9-]*):\s*$`).FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// links reads the cables, each normalised to an unordered pair so
// that writing an endpoint in the other order is not a difference.
func links(doc string) []string {
	_, after, ok := strings.Cut(doc, "\n  links:\n")
	if !ok {
		return nil
	}
	pair := regexp.MustCompile(`"([^"]+)"\s*,\s*"([^"]+)"`)
	var out []string
	for _, m := range pair.FindAllStringSubmatch(after, -1) {
		ends := []string{m[1], m[2]}
		sort.Strings(ends)
		out = append(out, ends[0]+" <-> "+ends[1])
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, "\n") == strings.Join(y, "\n")
}

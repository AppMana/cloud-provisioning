package cloudinit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/render"
)

// Every pattern the product ships must be a document this rig can
// apply, because the container rig stands in for the platform and
// applies it by hand.
//
// The patterns say in their own comments that they constrain
// themselves to write_files and runcmd for exactly this reason. That
// was a convention nothing enforced: a pattern could grow a
// packages: or a users: block, render perfectly, and be silently
// half-applied to a node that then looks healthy and fails somewhere
// unrelated. Rendering the real templates here turns the convention
// into a build failure.
//
// This is also what the harness being written in Go buys: it imports
// the product's own renderer, so the patterns under test are the ones
// that ship rather than copies that drift.
func TestEveryShippedPatternIsApplicableByThisRig(t *testing.T) {
	dir := patternDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var patterns []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".cloud-config.tmpl") && !strings.HasPrefix(e.Name(), "_") {
			patterns = append(patterns, e.Name())
		}
	}
	// Zero patterns is a failure, not a pass: an assertion that found
	// nothing to assert on proved nothing.
	if len(patterns) == 0 {
		t.Fatalf("no join patterns found under %s", dir)
	}

	for _, name := range patterns {
		t.Run(name, func(t *testing.T) {
			rendered, err := render.Pattern(filepath.Join(dir, name), patternValues())
			if err != nil {
				t.Fatalf("rendering: %v", err)
			}
			doc, err := Parse([]byte(rendered))
			if err != nil {
				t.Fatalf("the container rig cannot apply this pattern: %v", err)
			}
			// A pattern that writes nothing and runs nothing rendered
			// to something, but not to a join.
			if len(doc.WriteFiles) == 0 {
				t.Error("the pattern writes no files")
			}
			if len(doc.RunCmd) == 0 {
				t.Error("the pattern runs no commands")
			}
		})
	}
}

// Only the k0s pattern injects ssh_authorized_keys; kubeadm, k3s and
// RKE2 do not. That asymmetry is recorded here rather than asserted
// away in either direction, because it is a product question this
// harness is not the place to answer: either a remote should be
// reachable by SSH for an operator and three patterns are missing it,
// or it should not be and one pattern grants a login on an
// internet-facing node that the others deny.
//
// What this test does claim is that the harness knows about it. The
// container rig cannot install a login it never uses, so it reports
// the key as skipped; the bash harness ignored it silently, and the
// difference between what the pattern said and what a row proved was
// invisible.
func TestTheSSHKeyAsymmetryBetweenPatternsIsVisible(t *testing.T) {
	dir := patternDir(t)
	granting := map[string]bool{}
	for _, name := range []string{
		"kubeadm-worker.cloud-config.tmpl",
		"k0s-worker.cloud-config.tmpl",
		"k3s-worker.cloud-config.tmpl",
		"rke2-worker.cloud-config.tmpl",
	} {
		rendered, err := render.Pattern(filepath.Join(dir, name), patternValues())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		doc, err := Parse([]byte(rendered))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, skipped := range doc.Skipped {
			if skipped == "ssh_authorized_keys" {
				granting[name] = true
			}
		}
	}

	if !granting["k0s-worker.cloud-config.tmpl"] {
		t.Error("k0s no longer grants a login: if that was deliberate, this test and the note above should go")
	}
	for _, name := range []string{
		"kubeadm-worker.cloud-config.tmpl",
		"k3s-worker.cloud-config.tmpl",
		"rke2-worker.cloud-config.tmpl",
	} {
		if granting[name] {
			t.Errorf("%s now grants a login too, so the asymmetry is resolved: decide it deliberately and update this test", name)
		}
	}
}

// patternValues is a superset across providers. A pattern reads the
// keys it needs; the rest are inert. The values themselves are
// placeholders because what is under test is the document's shape,
// not its contents.
func patternValues() map[string]any {
	return map[string]any{
		"peersFileJSON":           "{}",
		"machineName":             "remote1",
		"interfaceName":           "cldt0",
		"wireguardListenPort":     "51820",
		"apiProxyPort":            7445,
		"apiEndpoint":             "10.10.0.10:6443",
		"joinEndpoint":            "10.10.0.10:6443",
		"joinToken":               "t.t",
		"caCertHash":              "sha256:x",
		"kubeletExtraArgs":        "",
		"sshAuthorizedKeys":       []string{"ssh-ed25519 AAAA test@harness"},
		"joinServerURL":           "https://10.10.0.10:6443",
		"k3sVersion":              "v1.34.0+k3s1",
		"rke2Version":             "v1.34.0+rke2r1",
		"k0sVersion":              "v1.34.0+k0s.0",
		"dialerBinaryURLArm64":    "https://example.invalid/a",
		"dialerBinarySHA256Arm64": "a",
		"dialerBinaryURLAmd64":    "https://example.invalid/b",
		"dialerBinarySHA256Amd64": "b",
		"cniPluginsURLArm64":      "https://example.invalid/c",
		"cniPluginsSHA256Arm64":   "c",
		"cniPluginsURLAmd64":      "https://example.invalid/d",
		"cniPluginsSHA256Amd64":   "d",
	}
}

// The values above must cover every key the patterns read. A pattern
// that grows one would otherwise fail here as a rendering error, and
// read as the pattern being broken rather than the test being stale.
func TestPatternValuesCoverEveryKeyTheTemplatesRead(t *testing.T) {
	dir := patternDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	action := regexp.MustCompile(`\{\{[^}]*\}\}`)
	field := regexp.MustCompile(`\.([a-zA-Z][a-zA-Z0-9_]*)`)

	have := patternValues()
	missing := map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tmpl") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, act := range action.FindAllString(string(body), -1) {
			// A template comment is prose, and prose names Go
			// identifiers: the patterns cite join.NodeLocalBalancer to
			// say why they carry no balancer port.
			if strings.Contains(act, "/*") {
				continue
			}
			for _, m := range field.FindAllStringSubmatch(act, -1) {
				if _, ok := have[m[1]]; !ok {
					missing[m[1]] = true
				}
			}
		}
	}
	for key := range missing {
		t.Errorf("the patterns read %q, which patternValues does not supply", key)
	}
}

// patternDir finds join-patterns/ from the module, so the test reads
// the shipped templates rather than a copy.
func patternDir(t *testing.T) string {
	t.Helper()
	// harness/e2e/cloudinit -> repository root
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "join-patterns"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("join-patterns not found at %s: %v", dir, err)
	}
	return dir
}

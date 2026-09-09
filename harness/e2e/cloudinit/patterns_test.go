package cloudinit

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/render"
)

// Cloud-config patterns must retain their file and command structure when
// parsed. This is a syntax/structure check, not proof that a container can run
// a distribution installer such as snapd. VM tests exercise native first boot.
func TestCloudConfigPatternsPreserveBootstrapStructure(t *testing.T) {
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
				t.Fatalf("invalid cloud-config pattern: %v", err)
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

// --join-ssh-authorized-keys says "on every new node". Every pattern
// must honour it, and the empty case must render nothing at all.
//
// This test previously recorded the opposite: only k0s read the value,
// so an operator who set the flag and ran kubeadm, k3s or RKE2 got no
// keys and no error. It was written to make the asymmetry visible
// rather than to bless it, and it failed the moment the patterns were
// fixed, which is what it was for.
//
// The container rig cannot install a login it never uses — it reaches
// nodes by docker exec — so it reports the key as skipped rather than
// applying it. That difference closes on the VM rig, where a real
// cloud-init reads the document.
func TestEveryPatternHonoursTheAuthorizedKeysFlag(t *testing.T) {
	dir := patternDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".cloud-config.tmpl") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		seen++
		values := patternValues()
		values["sshAuthorizedKeys"] = []string{"ssh-ed25519 AAAAKEY operator@example"}
		rendered, err := render.Pattern(filepath.Join(dir, e.Name()), values)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		doc, err := Parse([]byte(rendered))
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		var skipped bool
		for _, k := range doc.Skipped {
			if k == "ssh_authorized_keys" {
				skipped = true
			}
		}
		if !skipped {
			t.Errorf("%s does not honour --join-ssh-authorized-keys, so the flag silently does nothing there", e.Name())
		}

		// And with no keys, no key at all: a bare ssh_authorized_keys
		// is a null list, not an absent one.
		values["sshAuthorizedKeys"] = []string{}
		rendered, err = render.Pattern(filepath.Join(dir, e.Name()), values)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		if strings.Contains(rendered, "ssh_authorized_keys:") {
			t.Errorf("%s emits ssh_authorized_keys with no keys under it", e.Name())
		}
	}
	if seen == 0 {
		t.Fatal("no patterns were checked")
	}
}

// patternValues is a superset across providers. A pattern reads the
// keys it needs; the rest are inert. The values themselves are
// placeholders because what is under test is the document's shape,
// not its contents.
func patternValues() map[string]any {
	return map[string]any{
		"peersFileJSON":       "{}",
		"machineName":         "remote1",
		"interfaceName":       "cldt0",
		"wireguardListenPort": "51820",
		"apiProxyPort":        7445,
		"apiEndpoint":         "10.10.0.10:6443",
		"joinEndpoint":        "10.10.0.10:6443",
		"joinToken":           "t.t",
		"caCertHash":          "sha256:x",
		"kubeletExtraArgs":    "",
		// What the infrastructure provider contributes when it knows.
		// Present and empty rather than absent: the renderer refuses a
		// key it was never given, which is the behaviour that caught
		// this.
		"nodeAddress":             "",
		"providerID":              "",
		"sshAuthorizedKeys":       []string{"ssh-ed25519 AAAA test@harness"},
		"joinServerURL":           "https://10.10.0.10:6443",
		"k3sVersion":              "v1.34.0+k3s1",
		"rke2Version":             "v1.34.0+rke2r1",
		"k0sVersion":              "v1.34.0+k0s.0",
		"microk8sRevision":        "9063",
		"microk8sJoinURL":         "10.10.0.10:25000/0123456789abcdef0123456789abcdef/0123456789ab",
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

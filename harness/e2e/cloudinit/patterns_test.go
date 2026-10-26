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

// Every bootstrap pattern must honor --join-ssh-authorized-keys and omit
// keys when the value is empty. The container rig reports key installation
// as skipped because it uses docker exec; VM coverage exercises cloud-init.
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
		"wireguardAddress":    "100.64.0.2",
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
		// Empty unless the worker image carries a pinned, baked k0s; the
		// controller always supplies all three.
		"k0sWorkerVersion":     "",
		"k0sWorkerSHA256Amd64": "",
		"k0sWorkerSHA256Arm64": "",
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

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// A fresh deployment destroys the previous wrappers, so nothing a previous
// run left in a machine's seed describes the machines about to boot. A
// launch receipt naming a destroyed wrapper failed the next run's first
// claim with "VM wrapper identity changed", and a previous run's rendered
// userdata would boot a new remote into the old cluster.
func TestAFreshDeploymentStartsEveryMachineFromACleanSeed(t *testing.T) {
	r := New(lab.Default(), t.TempDir())
	dir := r.SeedDir("remote1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"launch.json":         `{"uid":"old","hash":"h","wrapperID":"gone","phase":"launched"}`,
		"reset-instance":      "",
		"extra-userdata.yaml": "#cloud-config\nruncmd: [old-cluster-join]\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.prepareSeed("remote1"); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"launch.json", "reset-instance"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s survived a fresh deployment", gone)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "extra-userdata.yaml"))
	if err != nil || string(raw) != "#cloud-config\n{}\n" {
		t.Errorf("userdata = %q, %v; want the empty document", raw, err)
	}
	for _, want := range []string{"extra-authorized-keys", "extra-setup.sh", "extra-network.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s missing: %v", want, err)
		}
	}
}

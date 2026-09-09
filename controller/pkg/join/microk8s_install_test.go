package join

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The fixture is the observed Snap Store assertion failure. The stub tests
// bounded retry and failure propagation, not Snap Store or CNI behavior.
func TestMicroK8sInstallRetriesObservedAssertionFailure(t *testing.T) {
	rendered := renderPatternWith(t, "microk8s-worker.cloud-config.tmpl", nil)
	var cfg struct {
		Files    []struct{ Path, Content string } `json:"write_files"`
		Commands []string                         `json:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &cfg); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, f := range cfg.Files {
		if f.Path == "/usr/local/sbin/cldt-install-microk8s" {
			script = f.Content
		}
	}
	if script == "" || !strings.Contains(strings.Join(cfg.Commands, "\n"), "/usr/local/sbin/cldt-install-microk8s") {
		t.Fatal("rendered bootstrap does not invoke installer")
	}
	failure, err := os.ReadFile("testdata/microk8s-snap-assertion-408.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		failures, attempts int
		succeeds           bool
	}{{"first attempt", 0, 1, true}, {"transient assertion failure", 2, 3, true}, {"persistent assertion failure", 99, 5, false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write("install.sh", script)
			write("failure", string(failure))
			write("snap", `#!/bin/sh
set -eu
[ "$*" = 'install microk8s --classic --revision=9063' ] || exit 87
n=0
[ ! -f "$TEST_DIR/count" ] || n=$(cat "$TEST_DIR/count")
n=$((n + 1))
printf '%s' "$n" > "$TEST_DIR/count"
if [ "$n" -le "$FAILURES" ]; then cat "$TEST_DIR/failure" >&2; exit 1; fi
`)
			write("sleep", `#!/bin/sh
[ "$1" = 15 ] || exit 88
printf 'sleep\n' >> "$TEST_DIR/sleeps"
`)
			cmd := exec.Command("sh", filepath.Join(dir, "install.sh"))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_DIR="+dir, "FAILURES="+strconv.Itoa(tc.failures))
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.succeeds {
				t.Fatalf("unexpected result: %v: %s", err, out)
			}
			count, err := os.ReadFile(filepath.Join(dir, "count"))
			if err != nil || string(count) != strconv.Itoa(tc.attempts) {
				t.Fatalf("attempts: %q %v", count, err)
			}
			sleeps, _ := os.ReadFile(filepath.Join(dir, "sleeps"))
			if strings.Count(string(sleeps), "sleep\n") != tc.attempts-1 {
				t.Fatal("retry interval/count mismatch")
			}
			if !tc.succeeds && !strings.Contains(string(out), "failed after 5 attempts") {
				t.Fatal("persistent failure was hidden")
			}
		})
	}
}

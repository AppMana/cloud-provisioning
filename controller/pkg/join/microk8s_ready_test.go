package join

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Native inspection found a joined worker with clustered.lock and kubelet
// healthz=ok while cluster-wide status --wait-ready remained in its old loop.
// These tests cover the worker gate, not the CNI or Kubernetes Ready condition.
func TestMicroK8sBootstrapWaitsForWorkerModeAndLocalKubelet(t *testing.T) {
	raw := renderPatternWith(t, "microk8s-worker.cloud-config.tmpl", nil)
	var config struct {
		Files    []struct{ Path, Content string } `json:"write_files"`
		Commands []string                         `json:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, f := range config.Files {
		if f.Path == "/usr/local/sbin/cldt-wait-microk8s-worker" {
			script = f.Content
		}
	}
	commands := strings.Join(config.Commands, "\n")
	if script == "" || !strings.Contains(commands, "timeout 600 /usr/local/sbin/cldt-wait-microk8s-worker") || strings.Contains(commands, "status --wait-ready") {
		t.Fatal("bootstrap still waits for cluster-wide status")
	}
	for _, tc := range []struct {
		name                   string
		lock, healthy, success bool
	}{{"joined and healthy", true, true, true}, {"not joined", false, true, false}, {"kubelet not healthy", true, false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lock := filepath.Join(dir, "clustered.lock")
			if tc.lock {
				if err := os.WriteFile(lock, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			body := strings.ReplaceAll(script, "/var/snap/microk8s/current/var/lock/clustered.lock", lock)
			if err := os.WriteFile(filepath.Join(dir, "wait.sh"), []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			curl := "#!/bin/sh\n[ \"$*\" = '-fsS --max-time 5 http://127.0.0.1:10248/healthz' ] || exit 89\n"
			if tc.healthy {
				curl += "printf ok\n"
			} else {
				curl += "exit 1\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/bin/sh\n[ \"$1\" = 2 ] || exit 88\nexec /bin/sleep 0.02\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("timeout", "0.2", "sh", filepath.Join(dir, "wait.sh"))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.success {
				t.Fatalf("worker gate: %v: %s", err, out)
			}
			if !tc.success {
				if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 124 {
					t.Fatalf("gate exited instead of waiting for worker readiness: %v", err)
				}
			}
		})
	}
}

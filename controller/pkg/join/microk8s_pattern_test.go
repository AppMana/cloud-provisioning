package join

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Native MicroK8s worker joining replaces args/kubelet after launch config is
// applied. Preserve the observed native labels and reapply product identity
// before kubelite starts, including after a later restart.
func TestMicroK8sIdentitySurvivesNativeArgumentRewrite(t *testing.T) {
	rendered := renderPatternWith(t, "microk8s-worker.cloud-config.tmpl", map[string]any{"apiProxyPort": 0})
	var config struct {
		Files []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		} `json:"write_files"`
	}
	if err := yaml.Unmarshal([]byte(rendered), &config); err != nil {
		t.Fatal(err)
	}
	var script, dropin string
	for _, f := range config.Files {
		switch f.Path {
		case "/usr/local/sbin/cldt-microk8s-kubelet":
			script = f.Content
		case "/etc/systemd/system/snap.microk8s.daemon-kubelite.service.d/10-cloud-provisioning.conf":
			dropin = f.Content
		}
	}
	if !strings.Contains(dropin, "ExecStartPre=/usr/local/sbin/cldt-microk8s-kubelet") {
		t.Fatal("identity must be installed before native kubelet registration")
	}
	dir := t.TempDir()
	desired := filepath.Join(dir, "desired")
	target := filepath.Join(dir, "kubelet")
	if err := os.WriteFile(desired, []byte("--node-labels=cloud-provisioning.appmana.com/role=cloud-worker\n--register-with-taints=cloud-provisioning.appmana.com/internet-facing:NoSchedule\n--provider-id=containernet://remote1\n--node-ip=203.0.113.10\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script = strings.ReplaceAll(script, "/etc/cloud-provisioning/microk8s-kubelet-args", desired)
	script = strings.ReplaceAll(script, "/var/snap/microk8s/current/args/kubelet", target)
	native := "--node-labels=microk8s.io/cluster=true,node.kubernetes.io/microk8s-worker=microk8s-worker\n--cluster-dns=10.152.183.10\n--kubeconfig=${SNAP_DATA}/credentials/kubelet.config\n"
	for restart := 0; restart < 2; restart++ {
		if err := os.WriteFile(target, []byte(native), 0600); err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			cmd := exec.Command("python3", "-c", script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("pre-start failed: %v: %s", err, out)
			}
		}
		body, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		got := string(body)
		for _, want := range []string{"microk8s.io/cluster=true", "node.kubernetes.io/microk8s-worker=microk8s-worker", "cloud-provisioning.appmana.com/role=cloud-worker", "--register-with-taints=cloud-provisioning.appmana.com/internet-facing:NoSchedule", "--provider-id=containernet://remote1", "--node-ip=203.0.113.10", "--cluster-dns=10.152.183.10", "--kubeconfig=${SNAP_DATA}/credentials/kubelet.config"} {
			if !strings.Contains(got, want) {
				t.Errorf("lost %s", want)
			}
		}
		if strings.Count(got, "--node-labels=") != 1 {
			t.Fatal("duplicate label flag")
		}
	}
}

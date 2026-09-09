package join

import (
	"os/exec"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// AWS's two address domains exposed a gap hidden by the fake VM's single
// reported address: pod traffic worked, but the API server could not reach
// the private EC2 InternalIP on 10250. Native probes reached the same kubelet
// through its allocated WireGuard address from all three control planes.
func TestMicroK8sAdvertisesTunnelAddressWithoutReplacingProviderIdentity(t *testing.T) {
	for _, tc := range []struct{ name, tunnel, physical, provider, want string }{
		{"observed AWS", "10.100.0.158/24", "172.29.0.45", "aws:///us-west-2a/i-worker", "10.100.0.158"},
		{"VM", "10.100.0.130/24", "203.0.113.10", "containernet://remote1", "10.100.0.130"},
		{"address unknown before creation", "10.100.0.157/24", "", "aws:///us-west-2a/i-new", "10.100.0.157"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := renderPatternWith(t, "microk8s-worker.cloud-config.tmpl", map[string]any{"wireguardAddress": tc.tunnel, "nodeAddress": tc.physical, "providerID": tc.provider})
			var config struct {
				Commands []string `json:"runcmd"`
			}
			if err := yaml.Unmarshal([]byte(raw), &config); err != nil {
				t.Fatal(err)
			}
			var assignments []string
			for _, command := range config.Commands {
				for _, line := range strings.Split(command, "\n") {
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "TUNNEL_NODE_ADDRESS=") || strings.HasPrefix(line, "NODE_IP_ARG=") || strings.HasPrefix(line, "PROVIDER_ID_ARG=") {
						assignments = append(assignments, line)
					}
				}
			}
			body := strings.Join(assignments, "\n") + "\nprintf '%s\\n%s' \"$NODE_IP_ARG\" \"$PROVIDER_ID_ARG\"\n"
			out, err := exec.Command("sh", "-ec", body).CombinedOutput()
			want := "--node-ip=" + tc.want + "\n--provider-id=" + tc.provider
			if err != nil || string(out) != want {
				t.Fatalf("identity args %q, error %v; want %q", out, err, want)
			}
			if strings.Contains(strings.Join(config.Commands, "\n"), "latest/meta-data/local-ipv4") {
				t.Fatal("MicroK8s still queries the private NIC address for kubelet advertisement")
			}
		})
	}
}

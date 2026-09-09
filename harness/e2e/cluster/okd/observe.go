package okd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
)

// ObserveMCS records reachability from existing host-network daemon pods without
// changing the installed firewall or retaining the credential-bearing response.
// An HTTP response here is only a network observation: curl deliberately skips
// certificate validation, so this does not establish a usable join transport.
func ObserveMCS(ctx context.Context, work string, k *kube.Client) error {
	const namespace = "openshift-machine-config-operator"
	raw, err := k.Run(ctx, "-n", namespace, "get", "pods", "-l", "k8s-app=machine-config-daemon", "-o", "json")
	if err != nil {
		return err
	}
	var pods corev1.PodList
	if err := json.Unmarshal(raw, &pods); err != nil {
		return err
	}
	type observation struct {
		Node, Pod, Output string
	}
	report := struct {
		ObservedAt  time.Time
		TLSVerified bool
		Results     []observation
	}{ObservedAt: time.Now().UTC()}
	for _, pod := range pods.Items {
		if !pod.Spec.HostNetwork || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		// Positional arguments keep API-provided addresses out of shell code.
		out, err := k.Run(ctx, "-n", namespace, "exec", pod.Name, "-c", "machine-config-daemon", "--", "bash", "-c", `
iptables-save | grep -E '22623|22624' || true
for host in 127.0.0.1 "$1" "$2"; do
  printf '%s ' "$host"
  curl --silent --show-error --insecure --output /dev/null \
    --write-out 'HTTP=%{http_code} bytes=%{size_download}\n' --max-time 5 \
    "https://$host:22623/config/worker" || true
done
`, "mcs-observe", pod.Status.HostIP, "api-int."+Domain)
		if err != nil {
			return fmt.Errorf("observing MCS from %s: %w", pod.Name, err)
		}
		report.Results = append(report.Results, observation{Node: pod.Spec.NodeName, Pod: pod.Name, Output: string(out)})
	}
	if len(report.Results) == 0 {
		return fmt.Errorf("no running host-network machine-config-daemon pods to observe")
	}
	raw, err = json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(work, "mcs-host-network.json"), raw, 0600)
}

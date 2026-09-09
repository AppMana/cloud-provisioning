package adoption

import (
	"context"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// Wait requires a current running adopter and an acknowledgment of the current
// peer document. It does not attribute that acknowledgment to a specific Pod.
func Wait(ctx context.Context, k *kube.Client, machine, node string, within time.Duration) (map[string]any, error) {
	var observation map[string]any
	err := wait.Until(ctx, within, "remote dialer adoption on "+node, func(ctx context.Context) error {
		raw, err := k.Run(ctx, "-n", "cloud-provisioning", "get", "daemonset", "cloud-provisioning-dialer-remote", "-o", "json")
		if err != nil {
			return err
		}
		ds, err := DecodeDaemonSet(raw)
		if err != nil {
			return err
		}
		raw, err = k.Run(ctx, "-n", "cloud-provisioning", "get", "pods", "--field-selector", "spec.nodeName="+node, "-o", "json")
		if err != nil {
			return err
		}
		pod, err := ReadyPod(ds, raw, node)
		if err != nil {
			return err
		}
		raw, err = k.Run(ctx, "-n", "cloud-provisioning", "get", "secret", tunnel.AdoptionSecretName(machine), "-o", "json")
		if err != nil {
			return err
		}
		receipt, err := AppliedReceipt(raw, machine)
		if err != nil {
			return err
		}
		observation = map[string]any{"daemonSetUID": ds.UID, "podUID": pod.UID, "containers": pod.Status.ContainerStatuses, "machine": machine, "peerReceipt": receipt}
		return nil
	})
	return observation, err
}

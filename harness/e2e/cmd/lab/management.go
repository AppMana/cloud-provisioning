package main

import (
	"context"
	"fmt"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
)

// Qualify every control plane separately: API failover and native guest
// execution must not conceal a broken kubelet path.
func qualifyManagement(ctx context.Context, k *kube.Client, namespace string, nodes []string, stage string) error {
	results := check.KubeletAccess(ctx, k, namespace, nodes)
	recordEvent("kubelet-access", map[string]any{"stage": stage, "namespace": namespace, "nodes": nodes, "results": results})
	if len(results) == 0 {
		return fmt.Errorf("%s: no kubelet management paths were checked", stage)
	}
	for _, result := range results {
		if !result.OK {
			return fmt.Errorf("%s: kubelet %s on %s through %s failed: %s", stage, result.Operation, result.Node, result.Server, result.Error)
		}
	}
	return nil
}

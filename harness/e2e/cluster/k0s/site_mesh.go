package k0s

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// siteSelector is every node that is not a provisioned remote: remotes
// register with the product's role label, site nodes carry none.
const siteSelector = `!has(cloud-provisioning.appmana.com/role)`

// SiteMeshObjects replace Calico's full node mesh with a mesh of the site's
// own nodes. The tunnel refuses BGP, so in the full mesh a remote's sessions
// never establish and Calico reports its node agent unready indefinitely,
// which stalls every rolling update of the DaemonSet. With no configured
// peers a remote is ready, and the site's routes are unchanged.
func SiteMeshObjects() []runtime.Object {
	config := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "crd.projectcalico.org/v1", "kind": "BGPConfiguration",
		"metadata": map[string]any{"name": "default"},
		"spec":     map[string]any{"nodeToNodeMeshEnabled": false},
	}}
	peer := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "crd.projectcalico.org/v1", "kind": "BGPPeer",
		"metadata": map[string]any{"name": "site-mesh"},
		"spec":     map[string]any{"nodeSelector": siteSelector, "peerSelector": siteSelector},
	}}
	return []runtime.Object{config, peer}
}

// CalicoNodesReady reports every named node without a Ready calico-node.
func CalicoNodesReady(ctx context.Context, k *kube.Client, nodes []string) error {
	raw, err := k.Run(ctx, "-n", "kube-system", "get", "pods", "-l", "k8s-app=calico-node", "-o", "json")
	if err != nil {
		return err
	}
	var list struct {
		Items []struct {
			Spec   struct{ NodeName string } `json:"spec"`
			Status struct {
				Conditions []struct{ Type, Status string } `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("reading calico-node pods: %w", err)
	}
	ready := map[string]bool{}
	present := map[string]bool{}
	for _, pod := range list.Items {
		present[pod.Spec.NodeName] = true
		for _, c := range pod.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready[pod.Spec.NodeName] = true
			}
		}
	}
	var missing, unready []string
	for _, n := range nodes {
		switch {
		case !present[n]:
			missing = append(missing, n)
		case !ready[n]:
			unready = append(unready, n)
		}
	}
	sort.Strings(missing)
	sort.Strings(unready)
	var problems []string
	if len(unready) > 0 {
		problems = append(problems, "calico-node is not Ready on "+strings.Join(unready, ","))
	}
	if len(missing) > 0 {
		problems = append(problems, "no calico-node runs on "+strings.Join(missing, ","))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

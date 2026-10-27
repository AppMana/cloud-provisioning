package k0s

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
)

// tunnelAgentSelector is the product's Calico agent on tunnel-class cloud
// nodes; the site's own calico-node runs everywhere else.
const tunnelAgentSelector = platform.ComponentLabel + "=calico-tunnel"

// CalicoNodesReady reports every named node without a Ready Calico agent of
// the right kind: the site's calico-node on a site node, the product's
// tunnel agent on a cloud node, and never the site's on a cloud node. An
// agent that is never ready stalls every rolling update of it.
func CalicoNodesReady(ctx context.Context, k *kube.Client, nodes []string) error {
	cloud, err := cloudNodes(ctx, k)
	if err != nil {
		return err
	}
	site, err := readyByNode(ctx, k, "k8s-app=calico-node")
	if err != nil {
		return err
	}
	tunnel, err := readyByNode(ctx, k, tunnelAgentSelector)
	if err != nil {
		return err
	}
	var problems []string
	for _, n := range nodes {
		want, other := site, tunnel
		kind := "the site's calico-node"
		if cloud[n] {
			want, other = tunnel, site
			kind = "the product's tunnel Calico agent"
		}
		ready, present := want[n]
		switch {
		case !present:
			problems = append(problems, fmt.Sprintf("%s does not run on %s", kind, n))
		case !ready:
			problems = append(problems, fmt.Sprintf("%s is not Ready on %s", kind, n))
		}
		if _, wrong := other[n]; wrong && cloud[n] {
			problems = append(problems, "the site's calico-node runs on cloud node "+n)
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// cloudNodes are the nodes the product provisioned.
func cloudNodes(ctx context.Context, k *kube.Client) (map[string]bool, error) {
	raw, err := k.Run(ctx, "get", "nodes", "-l", platform.RoleLabel, "-o", "json")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct{ Name string } `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("reading cloud nodes: %w", err)
	}
	cloud := map[string]bool{}
	for _, n := range list.Items {
		cloud[n.Metadata.Name] = true
	}
	return cloud, nil
}

// readyByNode maps each node running a matching pod to whether one is Ready.
func readyByNode(ctx context.Context, k *kube.Client, selector string) (map[string]bool, error) {
	raw, err := k.Run(ctx, "get", "pods", "-A", "-l", selector, "-o", "json")
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("reading %s pods: %w", selector, err)
	}
	ready := map[string]bool{}
	for _, pod := range list.Items {
		r := ready[pod.Spec.NodeName]
		for _, c := range pod.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				r = true
			}
		}
		ready[pod.Spec.NodeName] = r
	}
	return ready, nil
}

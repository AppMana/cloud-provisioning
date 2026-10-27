package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func nativePlugins(raw []byte) ([]string, error) {
	var config struct {
		Plugins []struct {
			Type string `json:"type"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("invalid CNI conflist")
	}
	types := []string{}
	for _, plugin := range config.Plugins {
		types = append(types, plugin.Type)
	}
	if len(types) == 0 || types[0] != "aws-cni" {
		return types, fmt.Errorf("primary CNI is not aws-cni")
	}
	seen := map[string]bool{}
	for _, name := range types {
		if seen[name] {
			return types, fmt.Errorf("duplicate CNI plugin %q", name)
		}
		seen[name] = true
		switch name {
		case "aws-cni", "egress-cni", "portmap":
		default:
			return types, fmt.Errorf("unexpected native CNI plugin %q", name)
		}
	}
	return types, nil
}

func cniPodOwnership(pods corev1.PodList, cloud map[string]bool, site map[string]bool) error {
	counts := map[string]int{}
	for _, pod := range pods.Items {
		agent := pod.Labels["k8s-app"]
		for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
			if strings.Contains(c.Image, "calico/") && cloud[pod.Spec.NodeName] {
				return fmt.Errorf("Calico container scheduled on AWS node %s", pod.Spec.NodeName)
			}
		}
		// The site network's agent is the site's calico-node, or on a
		// tunnel-class cloud node the product's calico-node-tunnel.
		if agent != "aws-node" && agent != "calico-node" && agent != "calico-node-tunnel" {
			continue
		}
		node := pod.Spec.NodeName
		if node == "" {
			return fmt.Errorf("unassigned %s pod", agent)
		}
		if (agent == "aws-node" && !cloud[node]) || (agent != "aws-node" && !site[node]) {
			return fmt.Errorf("%s scheduled outside its owned nodes: %s", agent, node)
		}
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			return fmt.Errorf("%s on %s is not stably running", agent, node)
		}
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		if !ready {
			return fmt.Errorf("%s on %s is not Ready", agent, node)
		}
		counts[node]++
	}
	for node := range cloud {
		if site[node] || counts[node] != 1 {
			return fmt.Errorf("AWS CNI ownership is ambiguous or missing on %s", node)
		}
	}
	for node := range site {
		if counts[node] != 1 {
			return fmt.Errorf("site CNI ownership is ambiguous or missing on %s", node)
		}
	}
	return nil
}

// A working workload is insufficient: reject competing CNI agents or runtime
// configurations even if the currently allocated pod still has connectivity.
// cniLabel is the class label the product registers a native-network node with.
const cniLabel = "cloud-provisioning.appmana.com/cni"

// nodeNetworks splits the measured nodes by the class the product registered
// them with: a provider-native network, or the site's.
func nodeNetworks(list corev1.NodeList, measured []string) (native, site map[string]bool) {
	native, site = map[string]bool{}, map[string]bool{}
	for _, node := range list.Items {
		if !slices.Contains(measured, node.Name) {
			continue
		}
		if node.Labels[cniLabel] != "" {
			native[node.Name] = true
		} else {
			site[node.Name] = true
		}
	}
	return native, site
}

// observedNodeNetworks reads nodeNetworks from the cluster.
func observedNodeNetworks(ctx context.Context, k *kube.Client, measured []string) (native, site map[string]bool, err error) {
	raw, err := k.Run(ctx, "get", "nodes", "-o", "json")
	if err != nil {
		return nil, nil, err
	}
	var list corev1.NodeList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, nil, err
	}
	native, site = nodeNetworks(list, measured)
	return native, site, nil
}

// awsNodeOwnedByProduct requires aws-node to be applied by the product's
// field manager and labelled as its own, so a hand-installed aws-node cannot
// stand in for the product's.
func awsNodeOwnedByProduct(ds appsv1.DaemonSet) error {
	if ds.Labels["app.kubernetes.io/managed-by"] != "cloud-provisioning" {
		return fmt.Errorf("aws-node is managed by %q, not cloud-provisioning", ds.Labels["app.kubernetes.io/managed-by"])
	}
	for _, m := range ds.ManagedFields {
		if m.Manager == "cloud-provisioning" && m.Operation == metav1.ManagedFieldsOperationApply {
			return nil
		}
	}
	return fmt.Errorf("aws-node has no fields applied by cloud-provisioning")
}

func verifyNativeCNI(ctx context.Context, k *kube.Client, fleet *rig.Fleet, cloud, site map[string]bool, reportDir string) error {
	raw, failure := k.Run(ctx, "-n", "kube-system", "get", "daemonset", "aws-node", "-o", "json", "--show-managed-fields")
	var awsNode appsv1.DaemonSet
	if failure == nil {
		failure = json.Unmarshal(raw, &awsNode)
	}
	if failure == nil {
		failure = awsNodeOwnedByProduct(awsNode)
	}
	var pods corev1.PodList
	if failure == nil {
		raw, failure = k.Run(ctx, "get", "pods", "-A", "-o", "json")
	}
	if failure == nil {
		failure = json.Unmarshal(raw, &pods)
	}
	if failure == nil {
		failure = cniPodOwnership(pods, cloud, site)
	}
	configurations := map[string][]string{}
	if failure == nil {
		for node := range cloud {
			// containerd chooses among these extensions. Require exactly one active
			// configuration, rather than reading only the expected file by name.
			raw, err := fleet.Node(node).Exec(ctx, "sh", "-ec", `count=0; selected=''; for f in /etc/cni/net.d/*.conf /etc/cni/net.d/*.conflist /etc/cni/net.d/*.json; do [ -f "$f" ] || continue; count=$((count+1)); selected="$f"; done; [ "$count" -eq 1 ]; cat "$selected"`)
			if err == nil {
				configurations[node], err = nativePlugins(raw)
			}
			if err != nil {
				failure = fmt.Errorf("CNI configuration on %s: %w", node, err)
				break
			}
		}
	}
	result := map[string]any{"passed": failure == nil, "cloudNodes": cloud, "siteNodes": site, "plugins": configurations}
	if failure != nil {
		result["error"] = failure.Error()
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(reportDir, "native-cni-ownership.json"), data, 0600); err != nil {
		return err
	}
	return failure
}

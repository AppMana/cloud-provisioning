// Package platform describes a cloud node by its class: how and where it is
// provisioned and which pod network runs on it. The class decides everything
// that differs between platforms (the labels and taint a node registers
// with, and which network owns its pods), so callers ask the class instead
// of branching on provider names.
package platform

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// RoleLabel/RoleValue mark every node this product provisions.
	RoleLabel = "cloud-provisioning.appmana.com/role"
	RoleValue = "cloud-worker"
	// TaintKey keeps ordinary site workloads off internet-facing cloud nodes.
	TaintKey = "cloud-provisioning.appmana.com/internet-facing"
	// CNILabel names a provider-native pod network on a node, and on a
	// machine template chooses it.
	CNILabel = "cloud-provisioning.appmana.com/cni"
	// SiteCNI chooses the site's own pod network over the tunnel. Nodes of
	// this class carry no CNILabel.
	SiteCNI = "site"
	// ComponentLabel marks a workload this product runs for a platform; the
	// site CNI exclusion never applies to one.
	ComponentLabel = "cloud-provisioning.appmana.com/component"
)

// Class is the platform tuple of one cloud node.
type Class struct {
	OS   string
	Arch string
	// CNI is a provider-native network name, or SiteCNI.
	CNI string
}

// ClassOf reads a node's class from its infrastructure machine, whose labels
// come from the machine template. defaultCNI is the platform's default
// network for a guest OS; nil, or an empty result, means the site CNI.
func ClassOf(machine *unstructured.Unstructured, defaultCNI func(os string) string) Class {
	labels := machine.GetLabels()
	c := Class{OS: labels[corev1.LabelOSStable], Arch: labels[corev1.LabelArchStable], CNI: labels[CNILabel]}
	// Templates that predate guest metadata describe Linux.
	if c.OS == "" {
		c.OS = "linux"
	}
	if c.CNI == "" && defaultCNI != nil {
		c.CNI = defaultCNI(c.OS)
	}
	if c.CNI == "" {
		c.CNI = SiteCNI
	}
	return c
}

// Native reports whether a provider-native network owns this node's pods.
func (c Class) Native() bool { return c.CNI != SiteCNI }

// NodeLabels are the labels a node of this class registers with.
func (c Class) NodeLabels() map[string]string {
	labels := map[string]string{RoleLabel: RoleValue}
	if c.Native() {
		labels[CNILabel] = c.CNI
	}
	return labels
}

// Taints are the taints a node of this class registers with.
func (c Class) Taints() []corev1.Taint {
	return []corev1.Taint{{Key: TaintKey, Effect: corev1.TaintEffectNoSchedule}}
}

// KubeletArgs is the only producer of a cloud node's --node-labels and
// --register-with-taints. extra carries every other kubelet flag.
func (c Class) KubeletArgs(extra string) string {
	labels := c.NodeLabels()
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+labels[k])
	}
	taints := make([]string, 0, len(c.Taints()))
	for _, t := range c.Taints() {
		taints = append(taints, t.Key+":"+string(t.Effect))
	}
	args := "--node-labels=" + strings.Join(pairs, ",") + " --register-with-taints=" + strings.Join(taints, ",")
	if extra = strings.TrimSpace(extra); extra != "" {
		args += " " + extra
	}
	return args
}

// ValidateExtraFlags rejects free-form kubelet flags that restate what the
// class decides.
func ValidateExtraFlags(flags string) error {
	for _, field := range strings.Fields(flags) {
		for _, owned := range []string{"--node-labels", "--register-with-taints"} {
			if field == owned || strings.HasPrefix(field, owned+"=") {
				return fmt.Errorf("%s is set by the node class, not by extra kubelet flags", owned)
			}
		}
	}
	return nil
}

// Network is a provider-native pod network a platform runs on its nodes.
type Network interface {
	// Name is the CNILabel value that selects it.
	Name() string
	// Supports reports whether it runs on this guest OS and architecture.
	// An empty arch means the template does not pin one.
	Supports(os, arch string) error
	// VerifyNode rejects a node whose state contradicts this network owning
	// its pods (for example a site CNI address pinned on it).
	VerifyNode(node *corev1.Node) error
}

// Index keys networks by name.
func Index(networks ...Network) map[string]Network {
	index := map[string]Network{}
	for _, n := range networks {
		index[n.Name()] = n
	}
	return index
}

// Check admits the class only when its platform provides the chosen
// network and runs it on this guest. The site CNI needs nothing from the
// platform.
func (c Class) Check(networks []Network) error {
	if !c.Native() {
		return nil
	}
	for _, n := range networks {
		if n.Name() == c.CNI {
			if err := n.Supports(c.OS, c.Arch); err != nil {
				return fmt.Errorf("pod network %q on %s/%s: %w", c.CNI, c.OS, c.Arch, err)
			}
			return nil
		}
	}
	return fmt.Errorf("this platform provides no pod network %q", c.CNI)
}

// Allocation is what a provider-native network assigned one node.
type Allocation struct {
	// PodPrefixes are the node's own pod addresses.
	PodPrefixes []netip.Prefix
	// NetworkID names the native network the node belongs to; peers in the
	// same network reach NativePrefixes without the tunnel.
	NetworkID      string
	NativePrefixes []netip.Prefix
}

// Observer reports a provider-native network's per-node allocations.
type Observer interface {
	Allocations(ctx context.Context, machine *unstructured.Unstructured, nodeName string) (Allocation, error)
}

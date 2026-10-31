package acceptance

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/site"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/tuple"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Options are the product settings the cluster is qualified against.
type Options struct {
	// TunnelMTU is the smallest observed tunnel MTU on the qualified path,
	// including the remote's egress-derived budget. The 1420 default requires
	// a sufficiently large underlay; smaller egresses require an explicit value.
	TunnelMTU int
	// TunnelIPv4 and TunnelIPv6 are the tunnel address ranges: the chart's
	// tunnel subnet and tunnel.ipv6Prefix.
	TunnelIPv4 string
	TunnelIPv6 string
	// Tuple is an optional platform tuple (e.g. "k0s + calico,bgp + aws") to
	// evaluate alongside cluster preconditions.
	Tuple string
	// K0sConfig is optional raw k0s.yaml or k0sctl.yaml bytes to validate.
	K0sConfig []byte `json:"-"`
	// ValuesYAML is optional Helm values.yaml bytes to inspect.
	ValuesYAML []byte `json:"-"`
	// ManifestsYAML is optional Kubernetes manifest YAML documents to inspect.
	ManifestsYAML [][]byte `json:"-"`
}

func (o Options) withDefaults() Options {
	if o.TunnelMTU == 0 {
		o.TunnelMTU = 1420
	}
	if o.TunnelIPv4 == "" {
		o.TunnelIPv4 = "10.100.0.0/24"
	}
	if o.TunnelIPv6 == "" {
		o.TunnelIPv6 = "fd00:10:100::/96"
	}
	return o
}

// Check is one precondition. A Required check that is not OK is a change
// the cluster needs before the product can work on it; the others report
// what was observed.
type Check struct {
	Name     string   `json:"name"`
	Required bool     `json:"required"`
	OK       bool     `json:"ok"`
	Detail   string   `json:"detail"`
	Warnings []string `json:"warnings,omitempty"`
}

// Passed reports whether every required check passed.
func Passed(checks []Check) bool {
	for _, c := range checks {
		if c.Required && !c.OK {
			return false
		}
	}
	return len(checks) > 0
}

var (
	calicoPools      = schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPoolList"}
	calicoIPAMBlocks = schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPAMBlockList"}
)

// Preconditions reads the cluster and reports what the product needs of
// it. It only reads; with a ReadOnly client it cannot do otherwise.
func Preconditions(ctx context.Context, c client.Reader, o Options) ([]Check, error) {
	o = o.withDefaults()
	tunnel4, err := netip.ParsePrefix(o.TunnelIPv4)
	if err != nil {
		return nil, fmt.Errorf("tunnel IPv4 range: %w", err)
	}
	tunnel6, err := netip.ParsePrefix(o.TunnelIPv6)
	if err != nil {
		return nil, fmt.Errorf("tunnel IPv6 range: %w", err)
	}
	var out []Check
	if c == nil {
		tupleChecks, err := tuple.Evaluate(ctx, nil, tuple.CheckOptions{
			Tuple:         o.Tuple,
			K0sConfig:     o.K0sConfig,
			ValuesYAML:    o.ValuesYAML,
			ManifestsYAML: o.ManifestsYAML,
			TunnelMTU:     o.TunnelMTU,
			TunnelIPv4:    o.TunnelIPv4,
			TunnelIPv6:    o.TunnelIPv6,
		})
		if err != nil {
			return nil, err
		}
		for _, tc := range tupleChecks {
			out = append(out, Check{
				Name:     tc.Name,
				Required: true,
				OK:       tc.Pass,
				Detail:   tc.Detail,
				Warnings: tc.Warnings,
			})
		}
		return out, nil
	}

	network, err := cni.Detect(ctx, c)
	if err != nil {
		return nil, err
	}
	out = append(out, Check{Name: "network", Required: true, OK: network.Encapsulation != cni.Unknown,
		Detail: fmt.Sprintf("%s, %s (%s)", network.Name, network.Encapsulation, network.Detail)})

	dualStack, err := network.DualStack(ctx, c)
	if err != nil {
		return nil, err
	}
	detail := "single-stack: the mesh carries IPv4 only"
	if dualStack {
		detail = "dual-stack: the product pairs IPv6 tunnel addresses from " + o.TunnelIPv6
	}
	out = append(out, Check{Name: "dual-stack", OK: true, Detail: detail})

	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	out = append(out, masquerade(ctx, c, network, nodes.Items))

	if network.Name == cni.Calico {
		ds := &appsv1.DaemonSet{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "calico-node"}, ds); err != nil {
			return nil, fmt.Errorf("reading kube-system/calico-node: %w", err)
		}
		out = append(out, podMTU(ctx, c, ds, o.TunnelMTU))
	}

	ranges := []netip.Prefix{tunnel4}
	if dualStack {
		ranges = append(ranges, tunnel6)
	}
	tunnelCheck, err := tunnelRanges(ctx, c, nodes.Items, ranges)
	if err != nil {
		return nil, err
	}
	out = append(out, tunnelCheck)
	if network.Name == cni.Calico {
		out = append(out, borrowedAddresses(ctx, c), bgpPeers(ctx, c))
	}
	out = append(out, endpointCandidates(nodes.Items))
	if strings.TrimSpace(o.Tuple) != "" || len(o.K0sConfig) > 0 || len(o.ValuesYAML) > 0 || len(o.ManifestsYAML) > 0 {
		tupleChecks, err := tuple.Evaluate(ctx, c, tuple.CheckOptions{
			Tuple:         o.Tuple,
			K0sConfig:     o.K0sConfig,
			ValuesYAML:    o.ValuesYAML,
			ManifestsYAML: o.ManifestsYAML,
			TunnelMTU:     o.TunnelMTU,
			TunnelIPv4:    o.TunnelIPv4,
			TunnelIPv6:    o.TunnelIPv6,
		})
		if err != nil {
			return nil, err
		}
		for _, tc := range tupleChecks {
			switch tc.Name {
			case "tuple", "k0s-config", "helm-values", "manifests", "calico-mtu", "calico-autodetection", "windows-bgp", "k0s-windows-egress", "site-cni-exclusion", "cloud-worker-workloads":
				out = append(out, Check{
					Name:     tc.Name,
					Required: true,
					OK:       tc.Pass,
					Detail:   tc.Detail,
					Warnings: tc.Warnings,
				})
			}
		}
	}
	return out, nil
}

// masquerade: every node's blocks must keep their pod source across the
// tunnel, which is the product's own check.
func masquerade(ctx context.Context, c client.Reader, network cni.Network, nodes []corev1.Node) Check {
	check := Check{Name: "masquerade", Required: true, OK: true}
	var blocks []string
	for _, n := range nodes {
		if n.Labels["cloud-provisioning.appmana.com/cni"] == "aws-vpc" {
			check.OK = false
			check.Detail = "AWS VPC CNI source preservation requires the separately scoped AWS family campaign; Calico IPAM cannot prove it"
			return check
		}
		prefixes, err := network.PrefixesFor(ctx, c, n.Name)
		if err != nil {
			if network.Encapsulation == cni.Native {
				blocks = append(blocks, n.Name+": none yet")
			}
			continue
		}
		if err := network.CheckMasquerade(ctx, c, prefixes); err != nil {
			check.OK = false
			check.Detail = err.Error()
			return check
		}
		for _, p := range prefixes {
			blocks = append(blocks, n.Name+" "+p.String())
		}
	}
	check.Detail = "pod sources preserved for " + strings.Join(blocks, ", ")
	return check
}

// podMTU compares the MTU Calico gives pod interfaces with what the
// tunnel carries. A pod sends datagrams up to its own MTU without
// fragmenting them; over a smaller tunnel the first one to each
// destination is lost and later ones are fragmented, and a sender that
// sets don't-fragment loses every one.
func podMTU(ctx context.Context, c client.Reader, ds *appsv1.DaemonSet, tunnelMTU int) Check {
	check := Check{Name: "pod-mtu", Required: true}
	if tunnelMTU < 1280 || tunnelMTU > 65535 {
		check.Detail = fmt.Sprintf("tunnel MTU %d is outside the usable dual-stack range 1280–65535", tunnelMTU)
		return check
	}
	mtu, source, err := cni.CalicoPodMTU(ctx, c, ds)
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	// Automatic or absent MTUs do not establish the pod interface budget.
	// Qualification requires an explicit value that also supports IPv6.
	if mtu < 1280 || mtu > 65535 {
		check.Detail = fmt.Sprintf("CNI MTU %d (%s) does not establish a usable dual-stack budget; configure an explicit MTU between 1280 and %d", mtu, source, tunnelMTU)
		return check
	}
	check.OK = mtu <= tunnelMTU
	check.Detail = fmt.Sprintf("pods get MTU %d (%s); the tunnel carries %d", mtu, source, tunnelMTU)
	if !check.OK {
		check.Detail += fmt.Sprintf("; set the CNI MTU to at most %d (k0s: spec.network.calico.mtu)", tunnelMTU)
	}
	return check
}

// tunnelRanges: the tunnel's ranges must be no address the cluster
// already uses, or a peer's accept list would claim it.
func tunnelRanges(ctx context.Context, c client.Reader, nodes []corev1.Node, ranges []netip.Prefix) (Check, error) {
	check := Check{Name: "tunnel-ranges", Required: true, OK: true}
	type used struct{ what, value string }
	var uses []used
	pools := &unstructured.UnstructuredList{}
	pools.SetGroupVersionKind(calicoPools)
	if err := c.List(ctx, pools); err == nil {
		for _, p := range pools.Items {
			cidr, _, _ := unstructured.NestedString(p.Object, "spec", "cidr")
			uses = append(uses, used{"pool " + p.GetName(), cidr})
		}
	} else if !absent(err) {
		return check, err
	}
	for _, n := range nodes {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
				uses = append(uses, used{n.Name + " " + string(a.Type), a.Address})
			}
		}
		for _, key := range []string{"projectcalico.org/IPv4Address", "projectcalico.org/IPv6Address"} {
			if v := n.Annotations[key]; v != "" {
				uses = append(uses, used{n.Name + " " + key, v})
			}
		}
		for _, cidr := range n.Spec.PodCIDRs {
			uses = append(uses, used{n.Name + " podCIDR", cidr})
		}
	}
	svc := &corev1.Service{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "kubernetes"}, svc); err == nil {
		for _, ip := range svc.Spec.ClusterIPs {
			uses = append(uses, used{"Service default/kubernetes", ip})
		}
	} else if !apierrors.IsNotFound(err) {
		return check, err
	}
	var clashes []string
	for _, u := range uses {
		prefix, err := parseAddressOrPrefix(u.value)
		if err != nil {
			continue
		}
		for _, r := range ranges {
			if prefix.Overlaps(r) {
				clashes = append(clashes, fmt.Sprintf("%s %s overlaps %s", u.what, u.value, r))
			}
		}
	}
	if len(clashes) > 0 {
		check.OK = false
		check.Detail = strings.Join(clashes, "; ")
		return check, nil
	}
	var names []string
	for _, r := range ranges {
		names = append(names, r.String())
	}
	check.Detail = fmt.Sprintf("%s clear of %d pools, node addresses and Service addresses", strings.Join(names, " and "), len(uses))
	return check, nil
}

func parseAddressOrPrefix(v string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(strings.TrimSpace(v)); err == nil {
		return p, nil
	}
	a, err := netip.ParseAddr(strings.TrimSpace(v))
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// borrowedAddresses: a pod whose address comes from a block affine to
// another node is routed by Calico as a host route, which no peer's
// accept list carries; across the tunnel it is unreachable.
func borrowedAddresses(ctx context.Context, c client.Reader) Check {
	check := Check{Name: "borrowed-addresses", OK: true}
	blocks := &unstructured.UnstructuredList{}
	blocks.SetGroupVersionKind(calicoIPAMBlocks)
	if err := c.List(ctx, blocks); err != nil {
		check.Detail = "IPAM blocks not readable: " + err.Error()
		return check
	}
	borrowed := map[string]int{}
	for _, b := range blocks.Items {
		affinity, _, _ := unstructured.NestedString(b.Object, "spec", "affinity")
		host := strings.TrimPrefix(affinity, "host:")
		attributes, _, _ := unstructured.NestedSlice(b.Object, "spec", "attributes")
		allocations, _, _ := unstructured.NestedSlice(b.Object, "spec", "allocations")
		for _, a := range allocations {
			index, ok := a.(int64)
			if !ok || index < 0 || int(index) >= len(attributes) {
				continue
			}
			attr, _ := attributes[index].(map[string]any)
			secondary, _ := attr["secondary"].(map[string]any)
			if node, _ := secondary["node"].(string); node != "" && node != host {
				borrowed[node]++
			}
		}
	}
	if len(borrowed) == 0 {
		check.Detail = fmt.Sprintf("no pod holds an address from another node's block (%d blocks)", len(blocks.Items))
		return check
	}
	var parts []string
	for node, n := range borrowed {
		parts = append(parts, fmt.Sprintf("%s: %d", node, n))
	}
	sort.Strings(parts)
	check.Detail = "pods on these nodes hold borrowed addresses, unreachable across a tunnel if the node is a remote: " + strings.Join(parts, ", ")
	return check
}

// bgpPeers: the tunnel carries no BGP, and Calico reports a node whose
// configured peers never establish as unready. The product replaces the
// full mesh with one of site nodes; peering the operator configured is
// theirs to scope, and the product refuses tunnel nodes until it does.
func bgpPeers(ctx context.Context, c client.Reader) Check {
	check := Check{Name: "bgp-peers", Required: true}
	reaching, err := site.PeersReachingCloudNodes(ctx, c)
	if err != nil {
		check.Detail = "BGP peers not readable: " + err.Error()
		return check
	}
	if len(reaching) > 0 {
		check.Detail = fmt.Sprintf("BGPPeer %s can peer with cloud nodes; scope its nodeSelector and peerSelector with %q", strings.Join(reaching, ", "), "!has(cloud-provisioning.appmana.com/role)")
		return check
	}
	check.OK = true
	check.Detail = "no BGP peer reaches a cloud node; the product limits the node mesh to site nodes"
	return check
}

func endpointCandidates(nodes []corev1.Node) Check {
	check := Check{Name: "endpoint-candidates", OK: true}
	var workers, planes []string
	for _, n := range nodes {
		if n.Labels["kubernetes.io/os"] == "windows" {
			continue
		}
		if platform.IsControlPlane(&n) {
			planes = append(planes, n.Name)
			continue
		}
		workers = append(workers, n.Name)
	}
	check.Detail = fmt.Sprintf("Linux workers %v can terminate tunnels by default; control planes %v only when tunnel.endpoints names them", workers, planes)
	return check
}

func absent(err error) bool {
	if apierrors.IsNotFound(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "no matches for kind") || strings.Contains(msg, "could not find the requested resource")
}

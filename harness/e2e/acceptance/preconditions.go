package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
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
	// TunnelMTU is the MTU of the tunnel devices: the endpoint's underlay
	// MTU less WireGuard's 80 bytes, which is 1420 on a 1500-byte LAN and
	// the remote dialer's fixed MTU.
	TunnelMTU int
	// TunnelIPv4 and TunnelIPv6 are the tunnel address ranges: the chart's
	// tunnel subnet and tunnel.ipv6Prefix.
	TunnelIPv4 string
	TunnelIPv6 string
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
	Name     string `json:"name"`
	Required bool   `json:"required"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
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
	calicoBGPPeers   = schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "BGPPeerList"}
	calicoBGPConfig  = schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "BGPConfiguration"}
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
		env := containerEnv(ds, "calico-node")
		out = append(out, autodetection("ipv4-autodetection", env["IP"], env["IP_AUTODETECTION_METHOD"], tunnel4))
		if dualStack {
			out = append(out, autodetection("ipv6-autodetection", env["IP6"], env["IP6_AUTODETECTION_METHOD"], tunnel6))
		}
		out = append(out, podMTU(ctx, c, ds, o.TunnelMTU))
		if env["CALICO_NETWORKING_BACKEND"] == "bird" {
			out = append(out, bgpMesh(ctx, c))
		}
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
	return out, nil
}

// masquerade: every node's blocks must keep their pod source across the
// tunnel, which is the product's own check.
func masquerade(ctx context.Context, c client.Reader, network cni.Network, nodes []corev1.Node) Check {
	check := Check{Name: "masquerade", Required: true, OK: true}
	var blocks []string
	for _, n := range nodes {
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

func containerEnv(ds *appsv1.DaemonSet, name string) map[string]string {
	env := map[string]string{}
	for _, c := range ds.Spec.Template.Spec.Containers {
		if c.Name != name {
			continue
		}
		// Later entries win, as they do for the container.
		for _, e := range c.Env {
			env[e.Name] = e.Value
		}
	}
	return env
}

// autodetection decides whether Calico's address detection, run on a
// remote, finds nothing and so keeps the address the product stores for
// it. Calico keeps a stored address only when detection fails; any
// method that finds an address on a remote replaces the tunnel address
// with one the site cannot reach, on every restart of calico-node.
func autodetection(name, mode, method string, tunnel netip.Prefix) Check {
	check := Check{Name: name, Required: true}
	switch strings.TrimSpace(mode) {
	case "none":
		check.OK = true
		check.Detail = "address detection is disabled for this family"
		return check
	case "":
		check.OK = true
		check.Detail = "no detection while a stored address exists, so the product's address stands"
		return check
	case "autodetect":
	default:
		check.Detail = fmt.Sprintf("a fixed address %q would be every node's, a remote's included", mode)
		return check
	}
	if method == "" {
		method = "first-found"
	}
	cidrs, ok := strings.CutPrefix(method, "cidr=")
	if !ok {
		check.Detail = fmt.Sprintf("method %q finds an address on a remote and replaces the tunnel address the product stores; use cidr= with the site's own ranges", method)
		return check
	}
	for _, raw := range strings.Split(cidrs, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			check.Detail = fmt.Sprintf("method %q: %v", method, err)
			return check
		}
		if prefix.Overlaps(tunnel) {
			check.Detail = fmt.Sprintf("method %q covers the tunnel range %s, so a remote detects its tunnel address itself and the site's own nodes can pick theirs", method, tunnel)
			return check
		}
	}
	check.OK = true
	check.Detail = fmt.Sprintf("method %q finds nothing on a remote outside %s (its cloud addresses must stay outside it too), so the stored tunnel address stands", method, cidrs)
	return check
}

// podMTU compares the MTU Calico gives pod interfaces with what the
// tunnel carries. A pod sends datagrams up to its own MTU without
// fragmenting them; over a smaller tunnel the first one to each
// destination is lost and later ones are fragmented, and a sender that
// sets don't-fragment loses every one.
func podMTU(ctx context.Context, c client.Reader, ds *appsv1.DaemonSet, tunnelMTU int) Check {
	check := Check{Name: "pod-mtu", Required: true}
	mtu, source, err := cniMTU(ctx, c, ds)
	if err != nil {
		check.Detail = err.Error()
		return check
	}
	check.OK = mtu <= tunnelMTU
	check.Detail = fmt.Sprintf("pods get MTU %d (%s); the tunnel carries %d", mtu, source, tunnelMTU)
	if !check.OK {
		check.Detail += fmt.Sprintf("; set the CNI MTU to at most %d (k0s: spec.network.calico.mtu)", tunnelMTU)
	}
	return check
}

func cniMTU(ctx context.Context, c client.Reader, ds *appsv1.DaemonSet) (int, string, error) {
	for _, container := range ds.Spec.Template.Spec.InitContainers {
		env := map[string]corev1.EnvVar{}
		for _, e := range container.Env {
			env[e.Name] = e
		}
		config, ok := env["CNI_NETWORK_CONFIG"]
		if !ok {
			continue
		}
		var doc struct {
			Plugins []struct {
				Type string          `json:"type"`
				MTU  json.RawMessage `json:"mtu"`
			} `json:"plugins"`
		}
		// The template's placeholders are strings, so it parses as JSON.
		if err := json.Unmarshal([]byte(config.Value), &doc); err != nil {
			return 0, "", fmt.Errorf("parsing %s's CNI_NETWORK_CONFIG: %w", container.Name, err)
		}
		for _, p := range doc.Plugins {
			if p.Type != "calico" {
				continue
			}
			var literal int
			if json.Unmarshal(p.MTU, &literal) == nil {
				return literal, "the calico plugin's mtu in " + container.Name + "'s CNI configuration", nil
			}
			var placeholder string
			if json.Unmarshal(p.MTU, &placeholder) != nil || placeholder != "__CNI_MTU__" {
				return 0, "", fmt.Errorf("the calico plugin's mtu %s is neither a number nor __CNI_MTU__", p.MTU)
			}
			mtuVar, ok := env["CNI_MTU"]
			if !ok {
				return 0, "", fmt.Errorf("the CNI configuration takes its MTU from CNI_MTU, which %s does not set", container.Name)
			}
			value := mtuVar.Value
			if ref := mtuVar.ValueFrom; ref != nil && ref.ConfigMapKeyRef != nil {
				cm := &corev1.ConfigMap{}
				if err := c.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ref.ConfigMapKeyRef.Name}, cm); err != nil {
					return 0, "", fmt.Errorf("reading the CNI MTU from ConfigMap %s: %w", ref.ConfigMapKeyRef.Name, err)
				}
				value = cm.Data[ref.ConfigMapKeyRef.Key]
			}
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return 0, "", fmt.Errorf("CNI_MTU %q is not a number", value)
			}
			return n, "CNI_MTU", nil
		}
	}
	return 0, "", fmt.Errorf("calico-node carries no CNI configuration to read the pod MTU from")
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

// bgpMesh: with Calico's full node mesh every node peers with every
// remote, the tunnel refuses those sessions, and Calico reports a node
// whose configured peers never establish as unready forever, so a remote's
// calico-node never becomes Ready and every rolling update of the
// DaemonSet stalls. Peering the site's nodes with each other only leaves a
// remote with no peers, which Calico reports ready.
func bgpMesh(ctx context.Context, c client.Reader) Check {
	check := Check{Name: "bgp-mesh", Required: true}
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(calicoBGPConfig)
	err := c.Get(ctx, types.NamespacedName{Name: "default"}, config)
	mesh := true
	switch {
	case err == nil:
		if enabled, found, _ := unstructured.NestedBool(config.Object, "spec", "nodeToNodeMeshEnabled"); found {
			mesh = enabled
		}
	case absent(err):
	default:
		check.Detail = "BGP configuration not readable: " + err.Error()
		return check
	}
	if mesh {
		check.Detail = "Calico's full node-to-node mesh is enabled, so every node peers with every remote across a tunnel that refuses BGP and remote calico-node pods are never Ready; disable it (BGPConfiguration default nodeToNodeMeshEnabled: false) and peer site nodes with each other (BGPPeer nodeSelector/peerSelector " + `"!has(cloud-provisioning.appmana.com/role)"` + ")"
		return check
	}
	peers := &unstructured.UnstructuredList{}
	peers.SetGroupVersionKind(calicoBGPPeers)
	if err := c.List(ctx, peers); err != nil && !absent(err) {
		check.Detail = "BGP peers not readable: " + err.Error()
		return check
	}
	for _, p := range peers.Items {
		node, _, _ := unstructured.NestedString(p.Object, "spec", "nodeSelector")
		peer, _, _ := unstructured.NestedString(p.Object, "spec", "peerSelector")
		if node != "" && peer != "" {
			check.OK = true
			check.Detail = fmt.Sprintf("node mesh disabled; %s peers nodes %q with %q", p.GetName(), node, peer)
			return check
		}
	}
	check.Detail = "node mesh disabled but no BGPPeer peers the site's nodes with each other, so the site has no routes"
	return check
}

func bgpPeers(ctx context.Context, c client.Reader) Check {
	check := Check{Name: "bgp-peers", OK: true}
	peers := &unstructured.UnstructuredList{}
	peers.SetGroupVersionKind(calicoBGPPeers)
	if err := c.List(ctx, peers); err != nil {
		if absent(err) {
			check.Detail = "no BGPPeer resources: the full node mesh only"
			return check
		}
		check.Detail = "BGP peers not readable: " + err.Error()
		return check
	}
	var names []string
	for _, p := range peers.Items {
		ip, _, _ := unstructured.NestedString(p.Object, "spec", "peerIP")
		names = append(names, p.GetName()+"="+ip)
	}
	check.Detail = fmt.Sprintf("%d explicit BGP peers %v; the tunnel refuses BGP, so none of them learns a remote's routes over it", len(names), names)
	return check
}

func endpointCandidates(nodes []corev1.Node) Check {
	check := Check{Name: "endpoint-candidates", OK: true}
	var workers, planes []string
	for _, n := range nodes {
		if n.Labels["kubernetes.io/os"] == "windows" {
			continue
		}
		if _, cp := n.Labels["node-role.kubernetes.io/control-plane"]; cp {
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

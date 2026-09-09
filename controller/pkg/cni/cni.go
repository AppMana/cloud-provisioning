// Package cni reads what the cluster's container network already
// records about each node, and answers one question: which prefixes must
// a peer be permitted in order to reach the pods on that node.
//
// The answer is per node, never cluster wide. WireGuard's accept list is
// a trie with a single owner per prefix, so configuring one prefix on
// two peers gives it to whichever was configured last. A cluster wide
// pod range written to every peer therefore lands on an arbitrary one.
// Each node's own blocks are disjoint by construction, which is the
// property that makes a mesh work at all.
//
// The answer also depends on how the network carries pod traffic. When
// it encapsulates, a packet crossing the tunnel is addressed to the
// node, so no pod prefix is needed. When it routes natively, the packet
// is addressed to the pod, so that node's blocks are needed and nothing
// else.
//
// Nothing here is configuration. Every value is read from the resources
// the network already maintains, because they are what it routes by.
package cni

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Encapsulation is how the network carries a pod packet between nodes.
type Encapsulation int

const (
	// Unknown means the network was not recognised. Callers must not
	// guess: an accept list that is too narrow drops traffic silently
	// and one that is too wide takes traffic that was not meant for it.
	Unknown Encapsulation = iota
	// Native routes pod addresses between nodes unchanged, so the
	// tunnel sees packets addressed to pods.
	Native
	// Encapsulated wraps pod packets in a node to node header, so the
	// tunnel only ever sees packets addressed to nodes.
	Encapsulated
)

func (e Encapsulation) String() string {
	switch e {
	case Native:
		return "native"
	case Encapsulated:
		return "encapsulated"
	}
	return "unknown"
}

// Network is the network this cluster runs, and how it reaches pods.
type Network struct {
	Name          string
	Encapsulation Encapsulation
	// Detail names the specific mechanism, for logs: the encapsulation
	// a network chose, or the reason it was taken to be native.
	Detail string
}

// Names of the networks understood here.
const (
	Calico        = "calico"
	Cilium        = "cilium"
	Flannel       = "flannel"
	KubeRouter    = "kube-router"
	OVNKubernetes = "ovn-kubernetes"
	Unrecognised  = "unknown"
)

var (
	calicoIPPoolList = schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPoolList"}
	calicoBlockList  = schema.GroupVersionKind{Group: "crd.projectcalico.org", Version: "v1", Kind: "BlockAffinityList"}
	ciliumNodeGVK    = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNode"}
)

// Detect and PrefixesFor take a Reader because they only ever read.
//
// Detect identifies the network from its own resources, in the order
// that avoids ambiguity: a network with its own address management is
// recognised by that, and only a cluster with none of them falls through
// to the per node allocations the controller manager writes.
func Detect(ctx context.Context, c client.Reader) (Network, error) {
	// The distribution operator explicitly names the selected network. This
	// must precede discovery from CRDs left by older standalone installations.
	if n, ok, err := detectOVN(ctx, c); err != nil {
		return Network{}, err
	} else if ok {
		return n, nil
	}
	// Canal first, ahead of Calico: canal installs calico's CRDs and
	// pools for its policy engine while pods route by flannel's
	// vxlan, so the pool check would classify it as native calico and
	// model routes the network does not have.
	if n, ok, err := detectCanal(ctx, c); err != nil {
		return Network{}, err
	} else if ok {
		return n, nil
	}
	if n, ok, err := detectCalico(ctx, c); err != nil {
		return Network{}, err
	} else if ok {
		return n, nil
	}
	if n, ok, err := detectCilium(ctx, c); err != nil {
		return Network{}, err
	} else if ok {
		return n, nil
	}
	if n, ok, err := detectFlannel(ctx, c); err != nil {
		return Network{}, err
	} else if ok {
		return n, nil
	}
	if n, ok, err := detectKubeRouter(ctx, c); err != nil {
		return Network{}, err
	} else if ok {
		return n, nil
	}
	return Network{Name: Unrecognised, Encapsulation: Unknown, Detail: "no recognised network configuration"}, nil
}

func detectOVN(ctx context.Context, c client.Reader) (Network, bool, error) {
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(schema.GroupVersionKind{Group: "operator.openshift.io", Version: "v1", Kind: "Network"})
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, config); err != nil {
		if meaningfulError(err) {
			return Network{}, false, fmt.Errorf("reading distribution network configuration: %w", err)
		}
		return Network{}, false, nil
	}
	kind, _, _ := unstructured.NestedString(config.Object, "spec", "defaultNetwork", "type")
	if kind != "OVNKubernetes" {
		return Network{}, false, nil
	}
	return Network{Name: OVNKubernetes, Encapsulation: Encapsulated, Detail: "distribution-managed OVN Geneve"}, true, nil
}

// detectCalico reads the encapsulation off the IP pools. A pool encapsulates
// when either mode is anything but Never, and CrossSubnet counts: whether it
// encapsulates depends on whether the two nodes share a subnet, and a node
// across a tunnel never does.
func detectCalico(ctx context.Context, c client.Reader) (Network, bool, error) {
	pools := &unstructured.UnstructuredList{}
	pools.SetGroupVersionKind(calicoIPPoolList)
	if err := c.List(ctx, pools); err != nil {
		if meaningfulError(err) {
			return Network{}, false, fmt.Errorf("listing Calico IP pools: %w", err)
		}
		return Network{}, false, nil
	}
	if len(pools.Items) == 0 {
		return Network{}, false, nil
	}
	for _, pool := range pools.Items {
		if disabled, _, _ := unstructured.NestedBool(pool.Object, "spec", "disabled"); disabled {
			continue
		}
		for _, field := range []string{"ipipMode", "vxlanMode"} {
			mode, _, _ := unstructured.NestedString(pool.Object, "spec", field)
			if mode != "" && mode != "Never" {
				return Network{
					Name:          Calico,
					Encapsulation: Encapsulated,
					Detail:        fmt.Sprintf("%s=%s on %s", field, mode, pool.GetName()),
				}, true, nil
			}
		}
	}
	return Network{Name: Calico, Encapsulation: Native, Detail: "no pool encapsulates"}, true, nil
}

func detectCilium(ctx context.Context, c client.Reader) (Network, bool, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "cilium-config"}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return Network{}, false, nil
		}
		return Network{}, false, fmt.Errorf("reading cilium-config: %w", err)
	}
	// routing-mode is the current key; tunnel/tunnel-protocol are what
	// older releases wrote, and an empty routing-mode means tunnel.
	switch strings.TrimSpace(cm.Data["routing-mode"]) {
	case "native":
		return Network{Name: Cilium, Encapsulation: Native, Detail: "routing-mode=native"}, true, nil
	case "tunnel":
		return Network{Name: Cilium, Encapsulation: Encapsulated, Detail: ciliumTunnelDetail(cm)}, true, nil
	}
	if legacy := strings.TrimSpace(cm.Data["tunnel"]); legacy == "disabled" {
		return Network{Name: Cilium, Encapsulation: Native, Detail: "tunnel=disabled"}, true, nil
	}
	return Network{Name: Cilium, Encapsulation: Encapsulated, Detail: ciliumTunnelDetail(cm)}, true, nil
}

func ciliumTunnelDetail(cm *corev1.ConfigMap) string {
	for _, key := range []string{"tunnel-protocol", "tunnel"} {
		if v := strings.TrimSpace(cm.Data[key]); v != "" {
			return key + "=" + v
		}
	}
	return "tunnel-protocol=vxlan"
}

// detectCanal recognises canal by its DaemonSet: flannel carrying
// calico's policy engine, deployed under the name canal (or RKE2's
// rke2-canal). It must run before the calico check, because canal's
// pools exist for policy while pods route by flannel; the
// encapsulation follows flannel's backend, vxlan unless canal's own
// config says otherwise.
func detectCanal(ctx context.Context, c client.Reader) (Network, bool, error) {
	for _, name := range []string{"rke2-canal", "canal"} {
		ds := &appsv1.DaemonSet{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: name}, ds); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return Network{}, false, fmt.Errorf("reading the %s DaemonSet: %w", name, err)
		}
		backend := "vxlan"
		for _, cmName := range []string{name + "-config", "canal-config"} {
			cm := &corev1.ConfigMap{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: cmName}, cm); err == nil {
				if b := jsonStringValue(cm.Data["net-conf.json"], "Type"); b != "" {
					backend = b
					break
				}
			}
		}
		if strings.EqualFold(backend, "host-gw") {
			return Network{Name: Flannel, Encapsulation: Native, Detail: "canal, Backend.Type=host-gw"}, true, nil
		}
		return Network{Name: Flannel, Encapsulation: Encapsulated, Detail: "canal, Backend.Type=" + backend}, true, nil
	}
	return Network{}, false, nil
}

func detectFlannel(ctx context.Context, c client.Reader) (Network, bool, error) {
	for _, ns := range []string{"kube-flannel", "kube-system"} {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kube-flannel-cfg"}, cm); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return Network{}, false, fmt.Errorf("reading kube-flannel-cfg: %w", err)
		}
		backend := jsonStringValue(cm.Data["net-conf.json"], "Type")
		if strings.EqualFold(backend, "host-gw") {
			return Network{Name: Flannel, Encapsulation: Native, Detail: "Backend.Type=host-gw"}, true, nil
		}
		if backend == "" {
			backend = "vxlan"
		}
		return Network{Name: Flannel, Encapsulation: Encapsulated, Detail: "Backend.Type=" + backend}, true, nil
	}
	// No ConfigMap does not mean no flannel: k3s's embedded flannel
	// runs in-process, with its configuration in a file on each node
	// and nothing of its own in the API. What every flannel leaves,
	// embedded or not, is its annotations on each node it serves, and
	// the backend type is among them.
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		if meaningfulError(err) {
			return Network{}, false, fmt.Errorf("listing nodes for flannel annotations: %w", err)
		}
		return Network{}, false, nil
	}
	for _, node := range nodes.Items {
		backend := node.Annotations["flannel.alpha.coreos.com/backend-type"]
		if backend == "" {
			continue
		}
		if strings.EqualFold(backend, "host-gw") {
			return Network{Name: Flannel, Encapsulation: Native, Detail: "node annotation backend-type=host-gw"}, true, nil
		}
		return Network{Name: Flannel, Encapsulation: Encapsulated, Detail: "node annotation backend-type=" + backend}, true, nil
	}
	return Network{}, false, nil
}

// detectKubeRouter recognises kube-router by its DaemonSet, and it is
// native regardless of the overlay arguments there.
//
// The encapsulation question here is what the tunnel sees, and
// kube-router's overlay never reaches it. It distributes routes over
// its BGP sessions alone, and its overlay encapsulates only along
// routes those sessions learned (a tunnel interface is created per
// injected route, from BGP best-path updates and nothing else); the
// mesh refuses BGP across its tunnels, so no route kube-router holds
// ever crosses one, and a pod packet the mesh carries arrives
// unwrapped whatever --enable-overlay and --overlay-type say. Those
// flags describe node pairs kube-router routes between itself, which
// across the tunnel is none.
func detectKubeRouter(ctx context.Context, c client.Reader) (Network, bool, error) {
	ds := &appsv1.DaemonSet{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "kube-router"}, ds); err != nil {
		if apierrors.IsNotFound(err) {
			return Network{}, false, nil
		}
		return Network{}, false, fmt.Errorf("reading the kube-router DaemonSet: %w", err)
	}
	return Network{
		Name:          KubeRouter,
		Encapsulation: Native,
		Detail:        "routes are distributed by BGP alone, which never crosses the tunnel, so pod packets do",
	}, true, nil
}

// PrefixesFor returns the prefixes a peer must be permitted so that pods
// on the named node are reachable through it.
//
// An encapsulated network returns none: its packets are addressed to the
// node, and the node's own addresses are carried separately. A native
// one returns that node's blocks, read from wherever the network records
// them.
func (n Network) PrefixesFor(ctx context.Context, c client.Reader, node string) ([]netip.Prefix, error) {
	if n.Encapsulation == Encapsulated {
		return nil, nil
	}
	if n.Encapsulation == Unknown {
		return nil, fmt.Errorf("the container network was not recognised, so the prefixes for node %s cannot be determined; configure them explicitly", node)
	}
	switch n.Name {
	case Calico:
		return calicoBlocksFor(ctx, c, node)
	case Cilium:
		if prefixes, err := ciliumBlocksFor(ctx, c, node); err == nil && len(prefixes) > 0 {
			return prefixes, nil
		}
	}
	return nodeBlocksFor(ctx, c, node)
}

// CheckMasquerade reports whether pod traffic to the given prefixes
// would have its source address rewritten on the way.
//
// Calico masquerades traffic leaving a pool whose natOutgoing is set,
// and decides what "leaving" means by whether the destination falls in
// any pool. A remote node's blocks come from the cluster's own pools,
// so ordinarily they do not, and this holds without anyone arranging
// it. It stops holding the moment a remote is allocated from somewhere
// else, and the failure is not a clean one: the traffic still arrives,
// with the endpoint's address as its source, so the reply goes to the
// node rather than the pod and the connection hangs instead of being
// refused. That is worth naming rather than leaving to be rediscovered.
func (n Network) CheckMasquerade(ctx context.Context, c client.Reader, prefixes []netip.Prefix) error {
	if n.Name != Calico || len(prefixes) == 0 {
		return nil
	}
	pools := &unstructured.UnstructuredList{}
	pools.SetGroupVersionKind(calicoIPPoolList)
	if err := c.List(ctx, pools); err != nil {
		if meaningfulError(err) {
			return fmt.Errorf("listing Calico IP pools: %w", err)
		}
		return nil
	}
	var masquerading []netip.Prefix
	for _, pool := range pools.Items {
		if nat, _, _ := unstructured.NestedBool(pool.Object, "spec", "natOutgoing"); !nat {
			continue
		}
		cidr, _, _ := unstructured.NestedString(pool.Object, "spec", "cidr")
		if prefix, err := netip.ParsePrefix(cidr); err == nil {
			masquerading = append(masquerading, prefix)
		}
	}
	if len(masquerading) == 0 {
		return nil
	}
	var covered []netip.Prefix
	for _, pool := range pools.Items {
		cidr, _, _ := unstructured.NestedString(pool.Object, "spec", "cidr")
		if prefix, err := netip.ParsePrefix(cidr); err == nil {
			covered = append(covered, prefix)
		}
	}
	for _, prefix := range prefixes {
		if containedByAny(prefix, covered) {
			continue
		}
		return fmt.Errorf("%s is outside every Calico IP pool while a pool masquerades outgoing traffic, so pod traffic to it would leave with a node's address and the replies would not come back to the pod; allocate the remote from a pool the cluster already has, or clear natOutgoing", prefix)
	}
	return nil
}

func containedByAny(prefix netip.Prefix, pools []netip.Prefix) bool {
	for _, pool := range pools {
		if pool.Overlaps(prefix) && pool.Bits() <= prefix.Bits() && pool.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

// calicoBlocksFor reads the block affinities Calico maintains, which are
// how it routes to that node itself. Only confirmed, undeleted blocks
// count: a pending one is not yet in use and a deleted one is not any
// more.
func calicoBlocksFor(ctx context.Context, c client.Reader, node string) ([]netip.Prefix, error) {
	blocks := &unstructured.UnstructuredList{}
	blocks.SetGroupVersionKind(calicoBlockList)
	if err := c.List(ctx, blocks); err != nil {
		return nil, fmt.Errorf("listing Calico block affinities: %w", err)
	}
	var out []netip.Prefix
	for _, block := range blocks.Items {
		if owner, _, _ := unstructured.NestedString(block.Object, "spec", "node"); owner != node {
			continue
		}
		if state, _, _ := unstructured.NestedString(block.Object, "spec", "state"); state != "confirmed" {
			continue
		}
		// spec.deleted is a string on this resource, not a boolean.
		if deleted, _, _ := unstructured.NestedString(block.Object, "spec", "deleted"); deleted == "true" {
			continue
		}
		cidr, _, _ := unstructured.NestedString(block.Object, "spec", "cidr")
		if prefix, err := netip.ParsePrefix(cidr); err == nil {
			out = append(out, prefix.Masked())
		}
	}
	return sorted(out), nil
}

func ciliumBlocksFor(ctx context.Context, c client.Reader, node string) ([]netip.Prefix, error) {
	ciliumNode := &unstructured.Unstructured{}
	ciliumNode.SetGroupVersionKind(ciliumNodeGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: node}, ciliumNode); err != nil {
		return nil, err
	}
	cidrs, _, _ := unstructured.NestedStringSlice(ciliumNode.Object, "spec", "ipam", "podCIDRs")
	var out []netip.Prefix
	for _, cidr := range cidrs {
		if prefix, err := netip.ParsePrefix(cidr); err == nil {
			out = append(out, prefix.Masked())
		}
	}
	return sorted(out), nil
}

// nodeBlocksFor reads the block the controller manager allocated, which
// is what a network without its own address management routes by.
func nodeBlocksFor(ctx context.Context, c client.Reader, name string) ([]netip.Prefix, error) {
	node := &corev1.Node{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, node); err != nil {
		return nil, fmt.Errorf("getting node %s: %w", name, err)
	}
	cidrs := node.Spec.PodCIDRs
	if len(cidrs) == 0 && node.Spec.PodCIDR != "" {
		cidrs = []string{node.Spec.PodCIDR}
	}
	if len(cidrs) == 0 {
		return nil, fmt.Errorf("node %s has no pod CIDR, and the network records none for it", name)
	}
	var out []netip.Prefix
	for _, cidr := range cidrs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return nil, fmt.Errorf("node %s has pod CIDR %q, which is not a prefix: %w", name, cidr, err)
		}
		out = append(out, prefix.Masked())
	}
	return sorted(out), nil
}

func sorted(prefixes []netip.Prefix) []netip.Prefix {
	sort.Slice(prefixes, func(i, j int) bool { return prefixes[i].String() < prefixes[j].String() })
	return prefixes
}

// meaningfulError separates a network that is not installed, which is
// the normal case for every network but the one in use, from a genuine
// failure to read.
func meaningfulError(err error) bool {
	return !apierrors.IsNotFound(err) && !meta(err)
}

func meta(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "no matches for kind") || strings.Contains(msg, "could not find the requested resource")
}

// jsonStringValue pulls one quoted string value out of a small
// configuration document, without imposing a schema on the rest of it.
func jsonStringValue(doc, key string) string {
	needle := `"` + key + `"`
	i := strings.Index(doc, needle)
	if i < 0 {
		return ""
	}
	rest := doc[i+len(needle):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return ""
	}
	rest = rest[colon+1:]
	open := strings.Index(rest, `"`)
	if open < 0 {
		return ""
	}
	rest = rest[open+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

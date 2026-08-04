// Package discover reads the facts about a cluster that a joining node
// needs, from the cluster itself.
//
// These are all properties the cluster already knows: where its API
// server is, which ranges it allocates pods and services from, and what
// its nodes' addresses look like. Asking an operator to restate them in
// values is asking them to keep a second copy in sync with the first,
// and every one of them is fatal to get wrong: a wrong service CIDR
// silently drops all service traffic from the joined node rather than
// erroring.
//
// Every function returns a value only when it can determine it. Callers
// treat "not determined" as a reason to require explicit configuration,
// never as a reason to guess.
package discover

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// APIServers returns every control-plane API address, as host:port.
//
// The kubernetes Service in the default namespace is the API server's
// own record of where it can be reached; its EndpointSlices carry one
// entry per control plane. A joining node needs all of them, not one:
// pointing it at a single control plane makes that node a single point
// of failure for a cluster that has three.
func APIServers(ctx context.Context, c client.Client) ([]string, error) {
	slices := &discoveryv1.EndpointSliceList{}
	if err := c.List(ctx, slices,
		client.InNamespace(metav1.NamespaceDefault),
		client.MatchingLabels{discoveryv1.LabelServiceName: "kubernetes"}); err != nil {
		return nil, fmt.Errorf("listing EndpointSlices for the kubernetes Service: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, slice := range slices.Items {
		port := 6443
		for _, p := range slice.Ports {
			if p.Port != nil {
				port = int(*p.Port)
				break
			}
		}
		for _, ep := range slice.Endpoints {
			for _, addr := range ep.Addresses {
				hp := net.JoinHostPort(addr, fmt.Sprint(port))
				if !seen[hp] {
					seen[hp] = true
					out = append(out, hp)
				}
			}
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("the kubernetes Service in the default namespace has no endpoints")
	}
	return out, nil
}

// ServiceCIDRs returns the ranges the cluster allocates service IPs
// from.
//
// Kubernetes 1.31 and later publish these as ServiceCIDR objects. On
// older clusters the API server does not expose its own configuration
// at all, so the range is read out of the error the allocator returns
// for an out-of-range address: a dry-run Service create is rejected
// with a message naming the valid range. That is a documented,
// descriptive error, and it costs one rejected request with no side
// effects.
func ServiceCIDRs(ctx context.Context, c client.Client) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "networking.k8s.io", Version: "v1", Kind: "ServiceCIDRList",
	})
	if err := c.List(ctx, list); err == nil && len(list.Items) > 0 {
		var out []string
		for _, item := range list.Items {
			cidrs, _, _ := unstructured.NestedStringSlice(item.Object, "spec", "cidrs")
			out = append(out, cidrs...)
		}
		if len(out) > 0 {
			sort.Strings(out)
			return out, nil
		}
	}
	return serviceCIDRsFromAllocator(ctx, c)
}

// serviceCIDRFromError matches the allocator's rejection message, which
// ends by naming the range it would have accepted.
func serviceCIDRsFromAllocator(ctx context.Context, c client.Client) ([]string, error) {
	probe := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cloud-provisioning-range-probe", Namespace: metav1.NamespaceDefault},
		Spec: corev1.ServiceSpec{
			// An address no cluster allocates from, so the request is
			// always rejected and the Service is never created.
			ClusterIP: "192.0.2.1",
			Ports:     []corev1.ServicePort{{Port: 443}},
		},
	}
	err := c.Create(ctx, probe, client.DryRunAll)
	if err == nil {
		// Accepted, which means 192.0.2.0/24 really is this cluster's
		// service range. Nothing was created (dry run), but there is
		// no range to report either.
		return nil, fmt.Errorf("service range probe was accepted, so the range could not be read from a rejection")
	}
	cidr, perr := parseServiceRange(err.Error())
	if perr != nil {
		return nil, fmt.Errorf("could not read the service range from the allocator (%v): %w", perr, err)
	}
	return []string{cidr}, nil
}

// parseServiceRange pulls the range out of the allocator's rejection,
// which ends by naming the range it would have accepted.
func parseServiceRange(msg string) (string, error) {
	const marker = "The range of valid IPs is "
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return "", fmt.Errorf("no range in message")
	}
	fields := strings.Fields(msg[idx+len(marker):])
	if len(fields) == 0 {
		return "", fmt.Errorf("no range after marker")
	}
	cidr := strings.TrimSuffix(strings.TrimSpace(fields[0]), ".")
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		return "", fmt.Errorf("%q is not a CIDR", cidr)
	}
	return cidr, nil
}

// PodCIDRs returns the ranges the cluster allocates pod addresses from,
// asking whichever CNI is installed.
//
// There is no single source: a cluster whose controller manager does
// the allocating records it on each Node, while a CNI with its own IPAM
// keeps it in that CNI's own configuration and leaves Node.spec.podCIDRs
// empty. Both are checked, and the CNIs are asked in turn.
func PodCIDRs(ctx context.Context, c client.Client) ([]string, error) {
	if cidrs, err := podCIDRsFromNodes(ctx, c); err == nil && len(cidrs) > 0 {
		return cidrs, nil
	}
	for _, source := range []func(context.Context, client.Client) ([]string, error){
		podCIDRsFromCalico,
		podCIDRsFromCilium,
		podCIDRsFromFlannel,
	} {
		if cidrs, err := source(ctx, c); err == nil && len(cidrs) > 0 {
			return cidrs, nil
		}
	}
	return nil, fmt.Errorf("no pod CIDR found on any Node, or in Calico, Cilium or Flannel configuration")
}

// podCIDRsFromNodes covers every CNI that lets the controller manager
// allocate per-node blocks: kubeadm defaults, Flannel, kube-router, and
// Calico configured with host-local IPAM.
func podCIDRsFromNodes(ctx context.Context, c client.Client) ([]string, error) {
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, node := range nodes.Items {
		cidrs := node.Spec.PodCIDRs
		if len(cidrs) == 0 && node.Spec.PodCIDR != "" {
			cidrs = []string{node.Spec.PodCIDR}
		}
		for _, cidr := range cidrs {
			// Per-node blocks are carved out of one cluster range, and
			// the joining node needs the whole range, not the blocks.
			super, err := supernet(cidr)
			if err != nil || seen[super] {
				continue
			}
			seen[super] = true
			out = append(out, super)
		}
	}
	sort.Strings(out)
	return out, nil
}

// podCIDRsFromCalico reads Calico's own IPAM configuration. Pools with
// no allocation disabled are the ones pods actually come from.
func podCIDRsFromCalico(ctx context.Context, c client.Client) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "crd.projectcalico.org", Version: "v1", Kind: "IPPoolList",
	})
	if err := c.List(ctx, list); err != nil {
		return nil, err
	}
	var out []string
	for _, pool := range list.Items {
		if disabled, _, _ := unstructured.NestedBool(pool.Object, "spec", "disabled"); disabled {
			continue
		}
		if cidr, ok, _ := unstructured.NestedString(pool.Object, "spec", "cidr"); ok && cidr != "" {
			out = append(out, cidr)
		}
	}
	sort.Strings(out)
	return out, nil
}

// podCIDRsFromCilium reads the cluster-pool ranges out of Cilium's
// ConfigMap, which is where its IPAM keeps them.
func podCIDRsFromCilium(ctx context.Context, c client.Client) ([]string, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kube-system", Name: "cilium-config"}, cm); err != nil {
		return nil, err
	}
	var out []string
	for _, key := range []string{"cluster-pool-ipv4-cidr", "cluster-pool-ipv6-cidr"} {
		for _, cidr := range strings.Fields(strings.ReplaceAll(cm.Data[key], ",", " ")) {
			if _, _, err := net.ParseCIDR(cidr); err == nil {
				out = append(out, cidr)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// podCIDRsFromFlannel reads the network out of Flannel's ConfigMap,
// for the case where the controller manager is not the allocator.
func podCIDRsFromFlannel(ctx context.Context, c client.Client) ([]string, error) {
	for _, ns := range []string{"kube-flannel", "kube-system"} {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: "kube-flannel-cfg"}, cm); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		var out []string
		for _, key := range []string{"Network", "IPv6Network"} {
			// net-conf.json is small and its shape is stable; the
			// values wanted are quoted CIDRs against these two keys.
			for _, cidr := range jsonStringValues(cm.Data["net-conf.json"], key) {
				if _, _, err := net.ParseCIDR(cidr); err == nil {
					out = append(out, cidr)
				}
			}
		}
		sort.Strings(out)
		if len(out) > 0 {
			return out, nil
		}
	}
	return nil, fmt.Errorf("no Flannel configuration found")
}

func jsonStringValues(doc, key string) []string {
	var out []string
	needle := `"` + key + `"`
	for rest := doc; ; {
		i := strings.Index(rest, needle)
		if i < 0 {
			return out
		}
		rest = rest[i+len(needle):]
		colon := strings.Index(rest, ":")
		if colon < 0 {
			return out
		}
		rest = rest[colon+1:]
		open := strings.Index(rest, `"`)
		if open < 0 {
			return out
		}
		rest = rest[open+1:]
		close := strings.Index(rest, `"`)
		if close < 0 {
			return out
		}
		out = append(out, rest[:close])
		rest = rest[close+1:]
	}
}

// NodeAddressRanges returns the ranges the cluster's own nodes carry
// addresses in, one per family.
//
// A remote node's real address belongs to its cloud provider, not to
// this cluster, so it is given a second address inside these ranges for
// the CNI to peer and autodetect on. Reading the ranges off the
// existing nodes keeps that allocation in the cluster's own address
// space without anyone having to name it.
func NodeAddressRanges(ctx context.Context, c client.Client) ([]netip.Prefix, error) {
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes); err != nil {
		return nil, err
	}
	var v4, v6 *netip.Prefix
	for _, node := range nodes.Items {
		for _, addr := range node.Status.Addresses {
			if addr.Type != corev1.NodeInternalIP {
				continue
			}
			ip, err := netip.ParseAddr(addr.Address)
			if err != nil {
				continue
			}
			// A /24 and a /64 are the smallest ranges that reliably
			// contain a site's nodes without reaching past them.
			bits, target := 24, &v4
			if ip.Is6() {
				bits, target = 64, &v6
			}
			prefix, err := ip.Prefix(bits)
			if err != nil {
				continue
			}
			if *target == nil {
				p := prefix.Masked()
				*target = &p
			}
		}
	}
	var out []netip.Prefix
	for _, p := range []*netip.Prefix{v4, v6} {
		if p != nil {
			out = append(out, *p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no node carries an InternalIP")
	}
	return out, nil
}

// supernet widens a per-node block to the range it was carved from.
// Controller-manager allocation hands out /24s from a /16 by default,
// and /64s from a /48; a joining node needs the whole range in its
// accept list, since it will talk to every node's pods, not just its
// own.
func supernet(cidr string) (string, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", err
	}
	bits := 16
	if prefix.Addr().Is6() {
		bits = 48
	}
	if prefix.Bits() <= bits {
		return prefix.Masked().String(), nil
	}
	wider, err := prefix.Addr().Prefix(bits)
	if err != nil {
		return "", err
	}
	return wider.Masked().String(), nil
}

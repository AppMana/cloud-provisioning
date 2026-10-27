package main

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Rebuild from authoritative hosts and this observation, never from the previous
// AllowedIPs: secondary pod IPs are /32s too and must disappear when released.
func nativeAllowedIPs(machine *unstructured.Unstructured, secret *corev1.Secret, prefixes []netip.Prefix) (string, error) {
	var hosts []string
	for _, key := range []string{join.WireGuardAddrAnnotation, join.WireGuardAddr6Annotation} {
		if raw := machine.GetAnnotations()[key]; raw != "" {
			p, err := netip.ParsePrefix(raw)
			if err != nil {
				return "", err
			}
			hosts = appendMissing(hosts, tunnel.HostCIDR(p.Addr().String()))
		}
	}
	if len(hosts) == 0 {
		return "", fmt.Errorf("Machine has no allocated tunnel host")
	}
	addresses, _, err := unstructured.NestedSlice(machine.Object, "status", "addresses")
	if err != nil {
		return "", err
	}
	_, machineAddresses := machineHosts(addresses)
	for _, ip := range machineAddresses {
		hosts = appendMissing(hosts, tunnel.HostCIDR(ip))
	}
	for _, p := range prefixes {
		hosts = appendMissing(hosts, p.String())
	}
	own := tunnel.PeerAllowedIPsPrefix + machine.GetName()
	for key, value := range secret.Data {
		if key == own || !strings.HasPrefix(key, tunnel.PeerAllowedIPsPrefix) {
			continue
		}
		for _, raw := range tunnel.SplitList(string(value)) {
			existing, err := netip.ParsePrefix(raw)
			if err != nil {
				return "", fmt.Errorf("invalid existing peer allocation")
			}
			for _, raw := range hosts {
				next, _ := netip.ParsePrefix(raw)
				if next.Overlaps(existing) {
					return "", fmt.Errorf("native allocation overlaps another mesh peer")
				}
			}
		}
	}
	// Site pod space is also an ownership boundary; it must never migrate to
	// a native peer because a provider range or allocation was misconfigured.
	for key, value := range secret.Data {
		if !strings.HasPrefix(key, tunnel.SitePodCIDRsPrefix) {
			continue
		}
		for _, raw := range tunnel.SplitList(string(value)) {
			site, err := netip.ParsePrefix(raw)
			if err != nil {
				return "", fmt.Errorf("invalid site allocation")
			}
			for _, p := range prefixes {
				if p.Overlaps(site) {
					return "", fmt.Errorf("native allocation overlaps site pod space")
				}
			}
		}
	}
	return strings.Join(hosts, ","), nil
}

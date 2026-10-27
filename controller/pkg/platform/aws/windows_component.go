package aws

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform/aws/windowscni"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunneldevice"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// WindowsCNIFacts are what a Windows VPC CNI node's plugin configuration
// depends on.
type WindowsCNIFacts struct {
	// ServiceRanges are the cluster's Service ranges; the plugin takes the
	// IPv4 one.
	ServiceRanges []netip.Prefix
	// ClusterDNS are the cluster DNS Service's addresses.
	ClusterDNS    []netip.Addr
	ClusterDomain string
	// SiteRanges are the site's pod, Service and node ranges, as for Linux.
	SiteRanges []netip.Prefix
	// PodMTU is the MTU of pod interfaces, at most the tunnel's.
	PodMTU int
}

// windowsSettings is the initializer's settings.json.
type windowsSettings struct {
	ServiceCIDR       string   `json:"serviceCIDR"`
	ClusterDNS        string   `json:"clusterDNS"`
	ClusterDomain     string   `json:"clusterDomain"`
	SNATExcludedCIDRs []string `json:"snatExcludedCIDRs"`
	HostRouteCIDRs    []string `json:"hostRouteCIDRs"`
	PodMTU            int      `json:"podMTU"`
}

const (
	windowsCNIName     = "aws-vpc-cni-windows"
	windowsCNIScripts  = windowsCNIName + "-scripts"
	windowsCNISettings = windowsCNIName + "-settings"
)

// WindowsCNIObjects are the Windows VPC CNI initializer's scripts and
// settings ConfigMaps and its DaemonSet. The initializer writes vpc-bridge's
// configuration once per node: SNAT stays off traffic to the site, and host
// routes send site-bound pod traffic to the host, whose tunnel carries it
// (the VPC's own router has no route to the site). The Service range is
// excluded from host routes; the plugin routes it itself. A node keeps the
// configuration it was given: the initializer refuses to replace a live
// one, so a change in the site reaches nodes that join after it.
func WindowsCNIObjects(f WindowsCNIFacts) (*corev1.ConfigMap, *corev1.ConfigMap, *appsv1.DaemonSet, error) {
	var services []netip.Prefix
	for _, p := range f.ServiceRanges {
		if p.Addr().Is4() {
			services = append(services, p)
		}
	}
	if len(services) != 1 {
		return nil, nil, nil, fmt.Errorf("the Windows VPC CNI takes one IPv4 Service range; the cluster has %v", f.ServiceRanges)
	}
	service := services[0]
	dns := slices.IndexFunc(f.ClusterDNS, func(a netip.Addr) bool { return a.Is4() })
	if dns < 0 || !service.Contains(f.ClusterDNS[dns]) {
		return nil, nil, nil, fmt.Errorf("the cluster DNS %v has no address in the IPv4 Service range %s", f.ClusterDNS, service)
	}
	if f.ClusterDomain == "" {
		return nil, nil, nil, fmt.Errorf("no cluster DNS domain")
	}
	if f.PodMTU < 1280 || f.PodMTU > tunneldevice.DefaultTransportMTU {
		return nil, nil, nil, fmt.Errorf("pod MTU %d must be between the IPv6 minimum 1280 and the tunnel's %d", f.PodMTU, tunneldevice.DefaultTransportMTU)
	}
	exclusions, err := snatExclusions(f.SiteRanges)
	if err != nil {
		return nil, nil, nil, err
	}
	settings := windowsSettings{ServiceCIDR: service.String(), ClusterDNS: f.ClusterDNS[dns].String(), ClusterDomain: f.ClusterDomain,
		SNATExcludedCIDRs: strings.Split(exclusions, ","), HostRouteCIDRs: []string{}, PodMTU: f.PodMTU}
	for _, raw := range settings.SNATExcludedCIDRs {
		if p := netip.MustParsePrefix(raw); !p.Overlaps(service) {
			settings.HostRouteCIDRs = append(settings.HostRouteCIDRs, raw)
		}
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, nil, nil, err
	}
	labels := func() map[string]string {
		return map[string]string{"app.kubernetes.io/managed-by": "cloud-provisioning", platform.ComponentLabel: VPCCNI{}.Name()}
	}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: "kube-system", Labels: labels()}
	}
	scripts := &corev1.ConfigMap{ObjectMeta: meta(windowsCNIScripts), Data: map[string]string{
		"configure-cni.ps1": windowscni.Configure, "verify-cni.ps1": windowscni.Verify, "cni-artifacts.json": windowscni.Recipe}}
	settingsMap := &corev1.ConfigMap{ObjectMeta: meta(windowsCNISettings), Data: map[string]string{"settings.json": string(raw)}}
	ds := &appsv1.DaemonSet{}
	if err := yaml.UnmarshalStrict(windowscni.DaemonSet, ds); err != nil {
		return nil, nil, nil, fmt.Errorf("the embedded Windows CNI DaemonSet: %w", err)
	}
	ds.Labels = labels()
	return scripts, settingsMap, ds, nil
}

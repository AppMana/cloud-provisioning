package aws

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"slices"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	corev1 "k8s.io/api/core/v1"
)

func windowsFacts() WindowsCNIFacts {
	prefixes := func(values ...string) []netip.Prefix {
		var out []netip.Prefix
		for _, v := range values {
			out = append(out, netip.MustParsePrefix(v))
		}
		return out
	}
	return WindowsCNIFacts{
		ServiceRanges: prefixes("10.96.0.0/12", "fd00:96::/108"),
		ClusterDNS:    []netip.Addr{netip.MustParseAddr("10.96.0.10"), netip.MustParseAddr("fd00:96::a")},
		ClusterDomain: "cluster.example",
		SiteRanges:    prefixes("10.244.0.0/16", "10.96.0.0/12", "10.10.0.10/32", "fd00:244::/56"),
		PodMTU:        1370,
	}
}

// A Windows VPC CNI node's plugin configuration comes from the site: its
// Service range and DNS, SNAT kept off every site range, host routes that
// send site-bound pod traffic to the host's tunnel (every site range but
// the Service range, which the plugin routes itself), and the tunnel's pod
// MTU. The recipe the initializer verifies is the pinned patched plugin
// build those settings need.
func TestWindowsCNISettingsComeFromTheSite(t *testing.T) {
	scripts, settings, ds, err := WindowsCNIObjects(windowsFacts())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(settings.Data["settings.json"]), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"serviceCIDR":       "10.96.0.0/12",
		"clusterDNS":        "10.96.0.10",
		"clusterDomain":     "cluster.example",
		"snatExcludedCIDRs": []any{"10.10.0.10/32", "10.96.0.0/12", "10.244.0.0/16"},
		"hostRouteCIDRs":    []any{"10.10.0.10/32", "10.244.0.0/16"},
		"podMTU":            float64(1370),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("settings:\n got %v\nwant %v", got, want)
	}

	var recipe struct{ Features []string }
	if err := json.Unmarshal([]byte(scripts.Data["cni-artifacts.json"]), &recipe); err != nil {
		t.Fatal(err)
	}
	for _, feature := range []string{"hostRouteCIDRs", "podMTU"} {
		if !slices.Contains(recipe.Features, feature) {
			t.Errorf("the pinned recipe lacks %s", feature)
		}
	}
	for _, key := range []string{"configure-cni.ps1", "verify-cni.ps1"} {
		if scripts.Data[key] == "" {
			t.Errorf("scripts lack %s", key)
		}
	}

	if ds.Name != "aws-vpc-cni-windows" || ds.Namespace != "kube-system" || ds.Labels[platform.ComponentLabel] != (VPCCNI{}).Name() {
		t.Fatalf("DaemonSet %s/%s labels %v", ds.Namespace, ds.Name, ds.Labels)
	}
	selector := ds.Spec.Template.Spec.NodeSelector
	if selector[corev1.LabelOSStable] != "windows" || selector[corev1.LabelArchStable] != "amd64" ||
		selector[platform.RoleLabel] != platform.RoleValue || selector[platform.CNILabel] != (VPCCNI{}).Name() {
		t.Fatalf("node selector %v", selector)
	}
	volumes := map[string]string{}
	for _, v := range ds.Spec.Template.Spec.Volumes {
		volumes[v.Name] = v.ConfigMap.Name
	}
	if volumes["scripts"] != scripts.Name || volumes["settings"] != settings.Name {
		t.Fatalf("volumes %v", volumes)
	}
}

// What the plugin cannot be configured from is refused.
func TestWindowsCNISettingsRefuseWhatCannotWork(t *testing.T) {
	for name, mutate := range map[string]func(*WindowsCNIFacts){
		"no IPv4 Service range":    func(f *WindowsCNIFacts) { f.ServiceRanges = f.ServiceRanges[1:] },
		"DNS outside the Services": func(f *WindowsCNIFacts) { f.ClusterDNS = []netip.Addr{netip.MustParseAddr("10.112.0.10")} },
		"no domain":                func(f *WindowsCNIFacts) { f.ClusterDomain = "" },
		"MTU over the tunnel's":    func(f *WindowsCNIFacts) { f.PodMTU = 1500 },
		"no site ranges":           func(f *WindowsCNIFacts) { f.SiteRanges = nil },
		"two IPv4 Service ranges": func(f *WindowsCNIFacts) {
			f.ServiceRanges = append(f.ServiceRanges, netip.MustParsePrefix("10.112.0.0/12"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := windowsFacts()
			mutate(&f)
			if _, _, _, err := WindowsCNIObjects(f); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

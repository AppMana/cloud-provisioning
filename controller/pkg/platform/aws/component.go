package aws

import (
	"bytes"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform/aws/upstream"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunneldevice"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// VPCCNIFacts are what the AWS VPC CNI's configuration depends on.
type VPCCNIFacts struct {
	// Family is the pod address family, ipv4 or ipv6.
	Family string
	// SiteRanges are the site's pod, Service and node ranges. Traffic from
	// AWS pods to them keeps its pod source address: it crosses the tunnel,
	// and the site routes replies to pods, not to the AWS node.
	SiteRanges []netip.Prefix
	// PodMTU is the MTU of AWS pod interfaces, at most the tunnel's.
	PodMTU int
	// Image and InitImage override the pinned upstream images.
	Image, InitImage string
}

var digestRE = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// VPCCNIObjects are the aws-node ConfigMap and DaemonSet for these facts:
// the pinned upstream render, scheduled onto this product's AWS VPC CNI
// nodes only, with one ENI per node (pods share the interface the tunnel
// uses) and SNAT kept off site-bound traffic.
func VPCCNIObjects(f VPCCNIFacts) (*corev1.ConfigMap, *appsv1.DaemonSet, error) {
	if f.Family != "ipv4" && f.Family != "ipv6" {
		return nil, nil, fmt.Errorf("VPC CNI family %q is neither ipv4 nor ipv6", f.Family)
	}
	if f.PodMTU < 1280 || f.PodMTU > tunneldevice.DefaultTransportMTU {
		return nil, nil, fmt.Errorf("pod MTU %d must be between the IPv6 minimum 1280 and the tunnel's %d", f.PodMTU, tunneldevice.DefaultTransportMTU)
	}
	snat, err := snatExclusions(f.SiteRanges)
	if err != nil {
		return nil, nil, err
	}
	for _, image := range []string{f.Image, f.InitImage} {
		if image != "" && !digestRE.MatchString(image) {
			return nil, nil, fmt.Errorf("VPC CNI image %q is not pinned by digest", image)
		}
	}

	configMap, ds, err := upstreamObjects()
	if err != nil {
		return nil, nil, err
	}
	labels := func(l map[string]string) map[string]string {
		delete(l, "helm.sh/chart")
		delete(l, "app.kubernetes.io/instance")
		l["app.kubernetes.io/managed-by"] = "cloud-provisioning"
		l[platform.ComponentLabel] = VPCCNI{}.Name()
		return l
	}
	configMap.Labels = labels(configMap.Labels)
	ds.Labels = labels(ds.Labels)

	spec := &ds.Spec.Template.Spec
	spec.NodeSelector = map[string]string{
		corev1.LabelOSStable: "linux",
		platform.RoleLabel:   platform.RoleValue,
		platform.CNILabel:    VPCCNI{}.Name(),
	}
	v6 := f.Family == "ipv6"
	env := map[string]string{
		"ENABLE_IPv4":                        strconv.FormatBool(!v6),
		"ENABLE_IPv6":                        strconv.FormatBool(v6),
		"ENABLE_PREFIX_DELEGATION":           strconv.FormatBool(v6),
		"AWS_VPC_K8S_CNI_CUSTOM_NETWORK_CFG": "false",
		"AWS_VPC_K8S_CNI_EXTERNALSNAT":       "false",
		"AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS": snat,
		"AWS_VPC_ENI_MTU":                    "1500",
		"POD_MTU":                            strconv.Itoa(f.PodMTU),
		"ENABLE_POD_ENI":                     "false",
		"ENABLE_MULTI_NIC":                   "false",
		"ENABLE_SUBNET_DISCOVERY":            "false",
		"MAX_ENI":                            "1",
		"WARM_ENI_TARGET":                    "0",
		"WARM_PREFIX_TARGET":                 "1",
	}
	unset := []string{}
	if v6 {
		unset = append(unset, "WARM_IP_TARGET", "MINIMUM_IP_TARGET")
	} else {
		env["WARM_IP_TARGET"] = "1"
		env["MINIMUM_IP_TARGET"] = "2"
	}
	for i := range spec.Containers {
		if spec.Containers[i].Name == "aws-node" {
			spec.Containers[i].Env = setEnv(spec.Containers[i].Env, env, unset)
			if f.Image != "" {
				spec.Containers[i].Image = f.Image
			}
		}
	}
	for i := range spec.InitContainers {
		if spec.InitContainers[i].Name == "aws-vpc-cni-init" {
			spec.InitContainers[i].Env = setEnv(spec.InitContainers[i].Env, map[string]string{"ENABLE_IPv6": strconv.FormatBool(v6)}, nil)
			if f.InitImage != "" {
				spec.InitContainers[i].Image = f.InitImage
			}
		}
	}
	return configMap, ds, nil
}

// snatExclusions is AWS_VPC_K8S_CNI_EXCLUDE_SNAT_CIDRS: the IPv4 site
// ranges, sorted. The VPC CNI does not SNAT IPv6 pods.
func snatExclusions(ranges []netip.Prefix) (string, error) {
	if len(ranges) == 0 {
		return "", fmt.Errorf("no site ranges to keep pod sources toward")
	}
	var v4 []netip.Prefix
	for _, p := range ranges {
		if p != p.Masked() || p.Bits() == 0 {
			return "", fmt.Errorf("site range %s is not a network", p)
		}
		for _, other := range v4 {
			if other.Overlaps(p) {
				return "", fmt.Errorf("site ranges %s and %s overlap", other, p)
			}
		}
		if p.Addr().Is4() {
			v4 = append(v4, p)
		}
	}
	sort.Slice(v4, func(i, j int) bool {
		if c := v4[i].Addr().Compare(v4[j].Addr()); c != 0 {
			return c < 0
		}
		return v4[i].Bits() < v4[j].Bits()
	})
	var out []string
	for _, p := range v4 {
		out = append(out, p.String())
	}
	return strings.Join(out, ","), nil
}

// setEnv sets each named variable in place, appends new ones in name
// order, and removes unset ones.
func setEnv(env []corev1.EnvVar, values map[string]string, unset []string) []corev1.EnvVar {
	seen := map[string]bool{}
	var out []corev1.EnvVar
	for _, e := range env {
		if contains(unset, e.Name) {
			continue
		}
		if v, ok := values[e.Name]; ok {
			e = corev1.EnvVar{Name: e.Name, Value: v}
			seen[e.Name] = true
		}
		out = append(out, e)
	}
	var added []string
	for name := range values {
		if !seen[name] {
			added = append(added, name)
		}
	}
	sort.Strings(added)
	for _, name := range added {
		out = append(out, corev1.EnvVar{Name: name, Value: values[name]})
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// upstreamObjects decodes the ConfigMap and DaemonSet from the pinned render.
func upstreamObjects() (*corev1.ConfigMap, *appsv1.DaemonSet, error) {
	var configMap *corev1.ConfigMap
	var ds *appsv1.DaemonSet
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(upstream.Manifest), 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := decoder.Decode(&u.Object); err == io.EOF {
			break
		} else if err != nil {
			return nil, nil, err
		}
		switch u.GetKind() {
		case "ConfigMap":
			configMap = &corev1.ConfigMap{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, configMap); err != nil {
				return nil, nil, err
			}
		case "DaemonSet":
			ds = &appsv1.DaemonSet{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ds); err != nil {
				return nil, nil, err
			}
		}
	}
	if configMap == nil || ds == nil {
		return nil, nil, fmt.Errorf("the pinned VPC CNI render has no aws-node ConfigMap and DaemonSet")
	}
	return configMap, ds, nil
}

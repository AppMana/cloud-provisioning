package aws

import (
	"fmt"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform/aws/upstream"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// WindowsIPAMFacts are what Windows IPAM depends on.
type WindowsIPAMFacts struct {
	// ClusterName is the CAPI Cluster whose AWSCluster names the VPC; its
	// name is the one the VPC CNI and CAPA tag that cluster's resources with.
	ClusterName, VPCID string
	// Image is the patched VPC resource controller (providers/aws-vpc-resource-controller).
	Image       string
	PullSecrets []string
}

// WindowsIPAMDeployment is AWS's VPC resource controller, patched to manage
// only this product's Windows VPC CNI nodes (--node-selector), placed on
// Linux VPC CNI workers with their network: IMDS there gives it its region,
// instance identity and the worker role, which the VPC CNI node policy
// already lets assign pod addresses on CAPA-owned interfaces. EKS's
// projected identity token and role are dropped. Replicas need separate
// workers: they bind host ports.
func WindowsIPAMDeployment(f WindowsIPAMFacts) (*appsv1.Deployment, error) {
	if !digestRE.MatchString(f.Image) {
		return nil, fmt.Errorf("Windows IPAM image %q is not pinned by digest", f.Image)
	}
	if f.ClusterName == "" || f.VPCID == "" {
		return nil, fmt.Errorf("Windows IPAM needs the AWS cluster's name and VPC")
	}
	d := &appsv1.Deployment{}
	if err := yaml.UnmarshalStrict(upstream.VPCResourceController, d); err != nil {
		return nil, fmt.Errorf("the pinned VPC resource controller Deployment: %w", err)
	}
	d.Labels["app.kubernetes.io/managed-by"] = "cloud-provisioning"
	d.Labels[platform.ComponentLabel] = VPCCNI{}.Name()
	spec := &d.Spec.Template.Spec
	spec.HostNetwork = true
	spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	spec.NodeSelector = map[string]string{
		corev1.LabelOSStable: "linux",
		platform.CNILabel:    VPCCNI{}.Name(),
		platform.RoleLabel:   platform.RoleValue,
	}
	spec.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": d.Spec.Template.Labels["app"]}},
			TopologyKey:   corev1.LabelHostname,
		}},
	}}
	spec.Tolerations = append(spec.Tolerations, corev1.Toleration{Key: platform.TaintKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
	spec.Volumes = slices.DeleteFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == "aws-iam-token" })
	for _, name := range f.PullSecrets {
		spec.ImagePullSecrets = append(spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
	}
	managed := strings.Join([]string{corev1.LabelOSStable + "=windows", platform.CNILabel + "=" + VPCCNI{}.Name(), platform.RoleLabel + "=" + platform.RoleValue}, ",")
	for i := range spec.Containers {
		if spec.Containers[i].Name != "controller" {
			continue
		}
		spec.Containers[i].Image = f.Image
		spec.Containers[i].Args = []string{"--cluster-name=" + f.ClusterName, "--vpc-id=" + f.VPCID, "--leader-elect",
			"--metrics-bind-address=:8443", "--introspect-bind-addr=:22775", "--node-selector=" + managed}
	}
	return d, nil
}

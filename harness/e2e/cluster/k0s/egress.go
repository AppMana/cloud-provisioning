package k0s

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
)

const DefaultRouteAgentName = "cldt-konnectivity-default-route-linux"
const DefaultRouteHealthPort = 18095
const DefaultRouteAdminPort = 18096

// DefaultRouteAgents derives a Linux-only addon from the actual distro agent.
// It deliberately leaves the native mixed-OS DaemonSet and host identifiers
// intact. Callers must verify host Service reachability and server strategies.
func DefaultRouteAgents(native []byte, hosts []string) ([]byte, error) {
	var ds appsv1.DaemonSet
	if err := json.Unmarshal(native, &ds); err != nil {
		return nil, fmt.Errorf("native agent: %w", err)
	}
	if ds.Kind != "DaemonSet" || ds.Name != "konnectivity-agent" || ds.Namespace != "kube-system" {
		return nil, fmt.Errorf("expected kube-system/konnectivity-agent DaemonSet")
	}
	hosts = slices.Clone(hosts)
	slices.Sort(hosts)
	if len(hosts) < 2 || len(slices.Compact(slices.Clone(hosts))) != len(hosts) {
		return nil, fmt.Errorf("at least two distinct Linux hostnames required")
	}
	for _, h := range hosts {
		if len(validation.IsValidLabelValue(h)) != 0 || h == "" {
			return nil, fmt.Errorf("invalid hostname label %q", h)
		}
	}
	pod := ds.Spec.Template.Spec.DeepCopy()
	if !pod.HostNetwork || pod.ServiceAccountName != "konnectivity-agent" || len(pod.Containers) != 1 || len(pod.InitContainers) != 0 {
		return nil, fmt.Errorf("unsupported native agent pod layout")
	}
	c := &pod.Containers[0]
	if c.Name != "konnectivity-agent" || c.Image == "" || len(c.Command) != 0 {
		return nil, fmt.Errorf("unsupported native agent container")
	}
	required := map[string]string{
		"--proxy-server-host": "localhost", "--proxy-server-port": "7132",
		"--agent-identifiers": "host=$(NODE_IP)", "--agent-id": "$(NODE_IP)",
	}
	seen := map[string]bool{}
	for i, arg := range c.Args {
		key, value, _ := strings.Cut(arg, "=")
		if key == "--health-server-port" || key == "--admin-server-port" {
			return nil, fmt.Errorf("native agent has custom listener ports")
		}
		if want, ok := required[key]; ok {
			if seen[key] || value != want {
				return nil, fmt.Errorf("unsupported native argument %s", key)
			}
			seen[key] = true
		}
		switch key {
		case "--agent-identifiers":
			c.Args[i] = "--agent-identifiers=default-route=true"
		case "--agent-id":
			c.Args[i] = "--agent-id=cldt-default-route-linux-$(NODE_IP)"
		}
	}
	if len(seen) != len(required) {
		return nil, fmt.Errorf("native agent missing required arguments")
	}
	nodeIP := false
	for _, e := range c.Env {
		if e.Name == "NODE_IP" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == "status.hostIP" {
			nodeIP = true
		}
	}
	if !nodeIP {
		return nil, fmt.Errorf("native agent missing downward host IP")
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil || c.LivenessProbe == nil || c.LivenessProbe.HTTPGet == nil {
		return nil, fmt.Errorf("native agent missing HTTP health probes")
	}
	c.Args = append(c.Args, fmt.Sprintf("--health-server-port=%d", DefaultRouteHealthPort), fmt.Sprintf("--admin-server-port=%d", DefaultRouteAdminPort))
	c.ReadinessProbe.HTTPGet.Port = intstr.FromInt32(DefaultRouteHealthPort)
	c.LivenessProbe.HTTPGet.Port = intstr.FromInt32(DefaultRouteHealthPort)
	if pod.SecurityContext != nil {
		pod.SecurityContext.WindowsOptions = nil
	}
	if c.SecurityContext != nil {
		c.SecurityContext.WindowsOptions = nil
	}
	pod.NodeName = ""
	pod.OS = &corev1.PodOS{Name: corev1.Linux}
	pod.NodeSelector = map[string]string{"kubernetes.io/os": "linux"}
	pod.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: hosts}}}}}}}
	labels := map[string]string{"app": DefaultRouteAgentName}
	addon := appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: DefaultRouteAgentName, Namespace: "kube-system"},
		Spec: appsv1.DaemonSetSpec{
			Selector:       &metav1.LabelSelector{MatchLabels: labels},
			Template:       corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: *pod},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType, RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: ptrInt(1), MaxSurge: ptrInt(0)}},
		},
	}
	return json.MarshalIndent(addon, "", "  ")
}

func ptrInt(n int32) *intstr.IntOrString { v := intstr.FromInt32(n); return &v }

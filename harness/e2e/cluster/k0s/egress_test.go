package k0s

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// Fixture is the observed k0s 1.36.2 DaemonSet, with runtime metadata removed.
// Native AWS Windows webhook failures and Linux-agent failover are recorded in
// docs/validation; these unit tests protect the configuration boundary only.
func nativeAgent(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/konnectivity-native.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestDefaultRoutePreservesNativeIdentityAndCredentials(t *testing.T) {
	raw := nativeAgent(t)
	var before appsv1.DaemonSet
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	out, err := DefaultRouteAgents(raw, []string{"cp3", "cp2"})
	if err != nil {
		t.Fatal(err)
	}
	var got appsv1.DaemonSet
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	p := got.Spec.Template.Spec
	c := p.Containers[0]
	n := before.Spec.Template.Spec
	if got.Name == before.Name || reflect.DeepEqual(got.Spec.Selector, before.Spec.Selector) {
		t.Fatal("addon collides with native agent ownership")
	}
	if c.Image != n.Containers[0].Image || !reflect.DeepEqual(p.Volumes, n.Volumes) || !reflect.DeepEqual(c.VolumeMounts, n.Containers[0].VolumeMounts) || !reflect.DeepEqual(c.Env, n.Containers[0].Env) || p.ServiceAccountName != n.ServiceAccountName {
		t.Fatal("distro image, projected credentials, or environment changed")
	}
	if p.OS == nil || p.OS.Name != corev1.Linux || p.NodeSelector["kubernetes.io/os"] != "linux" || p.SecurityContext.WindowsOptions != nil {
		t.Fatal("addon could use Windows host behavior")
	}
	hosts := p.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values
	if !reflect.DeepEqual(hosts, []string{"cp2", "cp3"}) {
		t.Fatalf("placement %v", hosts)
	}
	if c.ReadinessProbe.HTTPGet.Port.IntVal != 18095 || c.LivenessProbe.HTTPGet.Port.IntVal != 18095 {
		t.Fatal("listener collision")
	}
	if got.Spec.UpdateStrategy.RollingUpdate.MaxSurge.IntVal != 0 || got.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntVal != 1 {
		t.Fatal("unsafe host-network rolling update")
	}
	args := map[string]bool{}
	for _, a := range c.Args {
		args[a] = true
	}
	if !args["--agent-identifiers=default-route=true"] || !args["--agent-id=cldt-default-route-linux-$(NODE_IP)"] || args["--agent-id=$(NODE_IP)"] || !args["--admin-server-port=18096"] {
		t.Fatal("agent identity/listener isolation missing")
	}
	// Rendering must not modify the source bytes used for the ordinary agents.
	if !reflect.DeepEqual(raw, nativeAgent(t)) {
		t.Fatal("native bytes changed")
	}
}
func TestDefaultRouteRejectsUnsupportedNativeLayout(t *testing.T) {
	cases := map[string]func(*appsv1.DaemonSet){
		"name":        func(d *appsv1.DaemonSet) { d.Name = "other" },
		"hostnetwork": func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.HostNetwork = false },
		"sidecar": func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.Containers = append(d.Spec.Template.Spec.Containers, corev1.Container{Name: "sidecar"})
		},
		"command":        func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.Containers[0].Command = []string{"wrapper"} },
		"proxy endpoint": func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.Containers[0].Args[2] = "--proxy-server-host=remote" },
		"missing identity": func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.Containers[0].Args = d.Spec.Template.Spec.Containers[0].Args[:5]
		},
		"duplicate identity": func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.Containers[0].Args = append(d.Spec.Template.Spec.Containers[0].Args, "--agent-id=$(NODE_IP)")
		},
		"custom ports": func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.Containers[0].Args = append(d.Spec.Template.Spec.Containers[0].Args, "--health-server-port=8099")
		},
		"node IP": func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.Containers[0].Env = nil },
		"probe":   func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.Containers[0].ReadinessProbe = nil },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			var d appsv1.DaemonSet
			json.Unmarshal(nativeAgent(t), &d)
			change(&d)
			raw, _ := json.Marshal(d)
			if _, err := DefaultRouteAgents(raw, []string{"cp2", "cp3"}); err == nil {
				t.Fatal("accepted unsupported layout")
			}
		})
	}
	for _, hosts := range [][]string{nil, {"cp2"}, {"cp2", "cp2"}, {"cp2", ""}, {"cp2", "bad/host"}} {
		if _, err := DefaultRouteAgents(nativeAgent(t), hosts); err == nil {
			t.Fatalf("accepted %v", hosts)
		}
	}
	if _, err := DefaultRouteAgents([]byte("broken"), []string{"cp2", "cp3"}); err == nil {
		t.Fatal("accepted invalid JSON")
	}
}

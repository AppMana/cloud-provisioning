package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Exercise API defaulting and persisted revisions, which a fake client cannot
// model. Reconciling an admitted agent must not trigger another rollout.
func TestAgentDaemonSetsConvergeAfterImageUpgrade(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires isolated Kubernetes API binaries")
	}
	e := &envtest.Environment{}
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := windowsPublisherFixture()
	r.Client, r.reader = c, c
	r.secretName = "peers"
	r.dialerDaemonSetName = "site"
	r.dialerImage = "example.invalid/dialer:previous"
	r.dialerPrivateKeyDir = "/var/lib/cloud-provisioning"
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.secretNamespace}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		ensure  func(context.Context) error
		upgrade func()
	}{
		{"site", r.ensureDialerDaemonSet, func() { r.dialerImage = "example.invalid/dialer:site-next" }},
		{"remote", r.ensureCloudDialerDaemonSet, func() { r.dialerImage = "example.invalid/dialer:remote-next" }},
		{"remote-windows", r.ensureWindowsPublisherDaemonSet, func() { r.windowsPublisherImage = "example.invalid/publisher:next" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := func() *appsv1.DaemonSet {
				t.Helper()
				ds := &appsv1.DaemonSet{}
				if err := c.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: tc.name}, ds); err != nil {
					t.Fatal(err)
				}
				return ds
			}
			if err := tc.ensure(ctx); err != nil {
				t.Fatal(err)
			}
			before := read()
			if tc.name != "remote-windows" {
				assertAgentSysctlAccess(t, before)
				// Reconcile a previously deployed template without the host
				// sysctl mount; NET_ADMIN alone cannot write container /proc/sys.
				old := before.DeepCopy()
				for i, v := range old.Spec.Template.Spec.Volumes {
					if v.Name == "sysctl-net" {
						old.Spec.Template.Spec.Volumes = append(old.Spec.Template.Spec.Volumes[:i], old.Spec.Template.Spec.Volumes[i+1:]...)
						break
					}
				}
				agent := &old.Spec.Template.Spec.Containers[0]
				for i, m := range agent.VolumeMounts {
					if m.Name == "sysctl-net" {
						agent.VolumeMounts = append(agent.VolumeMounts[:i], agent.VolumeMounts[i+1:]...)
						break
					}
				}
				if err := c.Update(ctx, old); err != nil {
					t.Fatal(err)
				}
				if err := tc.ensure(ctx); err != nil {
					t.Fatal(err)
				}
				before = read()
				assertAgentSysctlAccess(t, before)
			}
			tc.upgrade()
			if err := tc.ensure(ctx); err != nil {
				t.Fatal(err)
			}
			after := read()
			if after.Generation != before.Generation+1 || after.Spec.Template.Spec.Containers[0].Image == before.Spec.Template.Spec.Containers[0].Image {
				t.Fatal("image upgrade did not change the admitted template exactly once")
			}
			for i := 0; i < 3; i++ {
				if err := tc.ensure(ctx); err != nil {
					t.Fatal(err)
				}
				current := read()
				if current.Generation != after.Generation || current.ResourceVersion != after.ResourceVersion {
					t.Fatalf("unchanged reconcile wrote a new revision: generation %d -> %d, resourceVersion %s -> %s", after.Generation, current.Generation, after.ResourceVersion, current.ResourceVersion)
				}
			}
			if tc.name != "remote-windows" {
				r.receivePreparation = &receivePreparationPolicy{Journal: &receiveJournalPolicy{Receipts: &receiveReceiptPolicy{Endpoint: "https://controller.test/v1/receipts", TrustSecret: "receipt-trust"}}}
				defer func() { r.receivePreparation = nil }()
				if err := tc.ensure(ctx); err != nil {
					t.Fatal(err)
				}
				configured := read()
				if configured.Spec.Template.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
					t.Fatal("host-network agent cannot resolve receipt Service through cluster DNS")
				}
				agent := configured.Spec.Template.Spec.Containers[0]
				if !strings.Contains(strings.Join(agent.Args, " "), "--receive-mesh-name=peers") {
					t.Fatal("agent lacks receipt dispatch configuration")
				}
				found := false
				for _, volume := range configured.Spec.Template.Spec.Volumes {
					if volume.Name != "receive-credentials" {
						continue
					}
					if volume.Projected == nil || len(volume.Projected.Sources) != 2 {
						t.Fatal("missing credential projections")
					}
					token, trust := volume.Projected.Sources[0].ServiceAccountToken, volume.Projected.Sources[1].Secret
					if token == nil || token.Audience != receiveReceiptAudience || token.Path != "token" || trust == nil || trust.Name != "receipt-trust" || len(trust.Items) != 1 || trust.Items[0].Key != "ca.crt" {
						t.Fatal("agent projection exposes wrong credential or serving private key")
					}
					found = true
				}
				if !found {
					t.Fatal("receipt projection absent")
				}
				if err := tc.ensure(ctx); err != nil {
					t.Fatal(err)
				}
				if read().ResourceVersion != configured.ResourceVersion {
					t.Fatal("receipt projection causes repeated rollouts")
				}
			}
		})
	}
}

func assertAgentSysctlAccess(t *testing.T, ds *appsv1.DaemonSet) {
	t.Helper()
	pod := ds.Spec.Template.Spec
	agent := pod.Containers[0]
	if !pod.HostNetwork || agent.SecurityContext == nil || (agent.SecurityContext.Privileged != nil && *agent.SecurityContext.Privileged) {
		t.Fatal("agent must use host networking without requiring privileged mode")
	}
	for _, mount := range agent.VolumeMounts {
		if mount.MountPath != tunnel.HostSysctlNet || mount.ReadOnly {
			continue
		}
		for _, volume := range pod.Volumes {
			if volume.Name == mount.Name && volume.HostPath != nil && volume.HostPath.Path == "/proc/sys/net" && volume.HostPath.Type != nil && *volume.HostPath.Type == corev1.HostPathDirectory {
				return
			}
		}
	}
	t.Fatal("agent cannot write the host network sysctls required by receive preparation")
}

package nodegroup

import (
	"context"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func verifyRegisteredGroups(t *testing.T, api client.Client, cfg *rest.Config) {
	t.Helper()
	namespace := "registered-groups"
	ctx, cancel := context.WithCancel(context.Background())
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: api.Scheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Register(mgr, namespace, "mesh", "", "6443"); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- mgr.Start(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-ended:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("manager did not stop")
		}
	}()
	if err := api.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	group := groupFixture()
	group.Namespace = namespace
	group.Name = "watched"
	group.UID = ""
	one := int32(1)
	group.Spec.Replicas = &one
	if err := api.Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-ended:
			t.Fatalf("manager exited: %v", err)
		case <-deadline:
			t.Fatal("registered controller did not create its child")
		case <-tick.C:
			children := &v1alpha1.ProvisionedNodeClaimList{}
			if err := api.List(ctx, children, client.InNamespace(namespace)); err != nil {
				t.Fatal(err)
			}
			if len(children.Items) == 1 {
				if _, err := ObserveChild(group, &children.Items[0]); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
}

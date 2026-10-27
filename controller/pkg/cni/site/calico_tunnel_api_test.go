package site

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// Against a real API server, which defaults what it stores: the agent made
// from k0s's calico-node is stable once created, so the controller does not
// roll it on every pass, and on a site without Calico's BGP resources the
// peering step has nothing to do. The registered controller brings up the
// agent from the site's DaemonSet and restores it after deletion.
func TestTheTunnelAgentConvergesAgainstTheAPI(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires isolated Kubernetes API binaries")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}})
	if err != nil {
		t.Fatal(err)
	}
	c := mgr.GetClient()
	e := &Excluder{Client: c, Reader: mgr.GetAPIReader(), Native: []string{"aws-vpc"}, Now: time.Now}
	if err := Register(mgr, e); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	if err := mgr.GetAPIReader().List(ctx, &appsv1.DaemonSetList{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, k0sCalicoNode(t)); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKey{Namespace: "kube-system", Name: "calico-node-tunnel"}
	agent := &appsv1.DaemonSet{}
	wait := func(what string, done func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !done() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	wait("the agent", func() bool { return mgr.GetAPIReader().Get(ctx, key, agent) == nil })

	generation := agent.Generation
	siteDS := &appsv1.DaemonSet{}
	if err := mgr.GetAPIReader().Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "calico-node"}, siteDS); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := e.Ensure(ctx, siteDS); err != nil {
			t.Fatal(err)
		}
	}
	if err := mgr.GetAPIReader().Get(ctx, key, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Generation != generation {
		t.Fatalf("the agent was rewritten on unchanged input: generation %d -> %d", generation, agent.Generation)
	}
	if err := e.Verify(ctx, cni.Network{Name: cni.Calico}, tunnelClass); err != nil {
		t.Fatalf("verify on a site without BGP resources: %v", err)
	}

	if err := c.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	wait("the agent to be restored", func() bool {
		restored := &appsv1.DaemonSet{}
		return mgr.GetAPIReader().Get(ctx, key, restored) == nil && restored.UID != agent.UID
	})
}

package main

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sync/atomic"
	"testing"
	"time"
)

type electedProbe struct {
	id              int
	entered         chan int
	active, overlap *atomic.Int32
}

func (p electedProbe) NeedLeaderElection() bool { return true }
func (p electedProbe) Start(ctx context.Context) error {
	if p.active.Add(1) != 1 {
		p.overlap.Add(1)
	}
	defer p.active.Add(-1)
	p.entered <- p.id
	<-ctx.Done()
	return nil
}
func TestMeshLeaderIdentity(t *testing.T) {
	if meshLeaderElectionID("mesh-a") != meshLeaderElectionID("mesh-a") || meshLeaderElectionID("mesh-a") == meshLeaderElectionID("mesh-b") {
		t.Fatal("lock does not follow mesh identity")
	}
}
func TestAPIMeshLeadershipSerializesWritersAndTransfers(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS for isolated API validation")
	}
	env := &envtest.Environment{}
	config, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	}()
	c, err := client.New(config, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "mesh-leadership"}}); err != nil {
		t.Fatal(err)
	}
	duration, renew, retry := 3*time.Second, 2*time.Second, 500*time.Millisecond
	var active, overlap atomic.Int32
	entered := make(chan int, 2)
	done := make(chan error, 2)
	cancels := make([]context.CancelFunc, 2)
	defer func() {
		for _, cancel := range cancels {
			if cancel != nil {
				cancel()
			}
		}
		for range 2 {
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("manager shutdown timeout")
			}
		}
	}()
	for id := 0; id < 2; id++ {
		mgr, err := ctrl.NewManager(config, ctrl.Options{Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0", LeaderElection: true, LeaderElectionNamespace: "mesh-leadership", LeaderElectionID: meshLeaderElectionID("shared-mesh"), LeaderElectionReleaseOnCancel: false, LeaseDuration: &duration, RenewDeadline: &renew, RetryPeriod: &retry})
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.Add(electedProbe{id, entered, &active, &overlap}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancels[id] = cancel
		go func() { done <- mgr.Start(ctx) }()
	}
	var first int
	select {
	case first = <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("no elected writer")
	}
	select {
	case <-entered:
		t.Fatal("two simultaneous elected writers")
	case <-time.After(time.Second):
	}
	cancels[first]()
	select {
	case second := <-entered:
		if second == first {
			t.Fatal("same writer entered twice")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("successor did not acquire expired lease")
	}
	if overlap.Load() != 0 {
		t.Fatal("overlapping resource writers")
	}
}

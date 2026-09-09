package aws

import (
	"context"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"testing"
)

type fixedResolver struct {
	binding ForwardingBinding
	fail    bool
}

func (r *fixedResolver) Resolve(context.Context, attachment.Record) (ForwardingBinding, error) {
	if r.fail {
		return r.binding, fmt.Errorf("Machine deleted")
	}
	return r.binding, nil
}

type guestProbe struct {
	ready    bool
	released int
}

func (g *guestProbe) Ensure(context.Context, attachment.Record, ForwardingBinding) (bool, error) {
	return g.ready, nil
}
func (g *guestProbe) Release(context.Context, attachment.Record, ForwardingBinding) (bool, error) {
	g.released++
	return g.ready, nil
}
func TestAttachmentForwarderWaitsForGuestAndReleasesWithoutMachine(t *testing.T) {
	ctx := context.Background()
	routes, cloud, j, route := routeFixture(t)
	base := j.RouteJournal.(ConfigMapJournal)
	checks, _, _, gateway := checkFixture(t)
	checks.API = cloud
	cloud.sourceCheck = true
	ingress, security, _, permission := ingressFixture(t)
	record := attachment.Record{ID: "worker", Lease: "epoch", Digest: "digest"}
	binding := ForwardingBinding{Lease: record.LeaseID(), Scope: routes.Scope, Gateway: gateway, Routes: []RouteTarget{route}, Ingress: []IngressTarget{permission}}
	resolver := &fixedResolver{binding: binding}
	guest := &guestProbe{}
	f := AttachmentForwarder{Resolver: resolver, Guest: guest, Bound: BoundResources{Store: ConfigMapBindingStore{Client: base.Client, Namespace: base.Namespace}, Resources: ForwardingResources{Scope: routes.Scope, Routes: routes, Checks: checks, Ingress: ingress}}}
	if ok, err := f.Ensure(ctx, record); err != nil || ok {
		t.Fatalf("cloud readiness bypassed guest: %v %v", ok, err)
	}
	guest.ready = true
	if ok, err := f.Ensure(ctx, record); err != nil || !ok {
		t.Fatal(err)
	}
	resolver.fail = true
	guest.ready = false
	if ok, err := f.Release(ctx, record); err != nil || ok || len(security.rules) != 1 || cloud.sourceCheck {
		t.Fatal("released cloud before guest withdrawal")
	}
	guest.ready = true
	if ok, err := f.Release(ctx, record); err != nil || !ok || len(security.rules) != 0 || !cloud.sourceCheck {
		t.Fatalf("cleanup required deleted Machine: %v %v", ok, err)
	}
}

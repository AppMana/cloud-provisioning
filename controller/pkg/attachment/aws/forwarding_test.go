package aws

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

type bundleRoutes struct {
	events *[]string
	fail   bool
}

func (r *bundleRoutes) Acquire(context.Context, string, RouteTarget) (bool, error) {
	*r.events = append(*r.events, "route+")
	if r.fail {
		return false, fmt.Errorf("route unavailable")
	}
	return true, nil
}
func (r *bundleRoutes) Release(context.Context, string, RouteTarget) (bool, error) {
	*r.events = append(*r.events, "route-")
	if r.fail {
		return false, fmt.Errorf("route unavailable")
	}
	return true, nil
}

type bundleChecks struct{ events *[]string }

func (r bundleChecks) Acquire(context.Context, string, InterfaceTarget) (bool, error) {
	*r.events = append(*r.events, "check+")
	return true, nil
}
func (r bundleChecks) Release(context.Context, string, InterfaceTarget) (bool, error) {
	*r.events = append(*r.events, "check-")
	return true, nil
}

type bundleIngress struct{ events *[]string }

func (r bundleIngress) Acquire(context.Context, string, IngressTarget) (bool, error) {
	*r.events = append(*r.events, "ingress+")
	return true, nil
}
func (r bundleIngress) Release(context.Context, string, IngressTarget) (bool, error) {
	*r.events = append(*r.events, "ingress-")
	return true, nil
}
func TestForwardingPartialFailureRetainsGatewayUntilCleanup(t *testing.T) {
	var events []string
	routes := &bundleRoutes{events: &events, fail: true}
	f := ForwardingResources{Routes: routes, Checks: bundleChecks{&events}, Ingress: bundleIngress{&events}}
	b := ForwardingBinding{Lease: "worker/epoch", Gateway: InterfaceTarget{InterfaceID: "eni", InstanceID: "instance"}, Routes: []RouteTarget{{InterfaceID: "eni", InstanceID: "instance"}}, Ingress: []IngressTarget{{GroupID: "sg"}}}
	ctx := context.Background()
	if ok, err := f.Ensure(ctx, b); ok || err == nil {
		t.Fatal("partial setup declared ready")
	}
	if !reflect.DeepEqual(events, []string{"check+", "ingress+", "route+"}) {
		t.Fatal(events)
	}
	events = nil
	if ok, err := f.Release(ctx, b); ok || err == nil {
		t.Fatal("incomplete cleanup declared complete")
	}
	if !reflect.DeepEqual(events, []string{"ingress-", "route-"}) {
		t.Fatal("restored check before routes retired", events)
	}
	events = nil
	routes.fail = false
	if ok, err := f.Release(ctx, b); !ok || err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"ingress-", "route-", "check-"}) {
		t.Fatal(events)
	}
	events = nil
	b.Routes[0].InstanceID = "replacement"
	if _, err := f.Ensure(ctx, b); err == nil || len(events) != 0 {
		t.Fatal("mutated mismatched binding")
	}
}

func TestForwardingCompositionPreservesAllSharedResources(t *testing.T) {
	ctx := context.Background()
	routes, cloud, routeJournal, route := routeFixture(t)
	checks, _, checkJournal, gateway := checkFixture(t)
	checks.API = cloud
	cloud.sourceCheck = true
	ingress, security, _, permission := ingressFixture(t)
	f := ForwardingResources{Scope: routes.Scope, Routes: routes, Checks: checks, Ingress: ingress}
	b := ForwardingBinding{Scope: routes.Scope, Lease: "2022", Gateway: gateway, Routes: []RouteTarget{route}, Ingress: []IngressTarget{permission}}
	for _, lease := range []string{"2022", "2025"} {
		b.Lease = lease
		if ok, err := f.Ensure(ctx, b); err != nil || !ok {
			t.Fatalf("ensure %v %v", ok, err)
		}
	}
	b.Lease = "2022"
	if ok, err := f.Release(ctx, b); err != nil || !ok {
		t.Fatal(err)
	}
	rr, err := routeJournal.Load(ctx, routes.key(route))
	if err != nil || !rr.Leases["2025"] {
		t.Fatal("route lease lost")
	}
	cr, err := checkJournal.Load(ctx, checks.key(gateway))
	if err != nil || !cr.Leases["2025"] || cloud.sourceCheck || len(security.rules) != 1 {
		t.Fatal("shared forwarding lost")
	}
	b.Lease = "2025"
	if ok, err := f.Release(ctx, b); err != nil || !ok {
		t.Fatal(err)
	}
	rr, err = routeJournal.Load(ctx, routes.key(route))
	if err != nil || rr.Phase != "Absent" || !cloud.sourceCheck || len(security.rules) != 0 {
		t.Fatal("shared cleanup incomplete")
	}
}

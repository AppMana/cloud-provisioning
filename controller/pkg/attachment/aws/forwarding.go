package aws

import (
	"context"
	"errors"
	"fmt"
)

// ForwardingBinding must be persisted with the attachment before Ensure. Its
// values are resolved from verified CAPI/EC2 identities, never recomputed from
// replacement Machines during cleanup. The hosting Forwarder also observes
// guest routing and consumer readiness; EC2 resource readiness alone is not it.
type ForwardingBinding struct {
	Lease   string          `json:"lease"`
	Scope   Scope           `json:"scope"`
	Gateway InterfaceTarget `json:"gateway"`
	Routes  []RouteTarget   `json:"routes"`
	Ingress []IngressTarget `json:"ingress"`
}
type RouteLeases interface {
	Acquire(context.Context, string, RouteTarget) (bool, error)
	Release(context.Context, string, RouteTarget) (bool, error)
}
type CheckLeases interface {
	Acquire(context.Context, string, InterfaceTarget) (bool, error)
	Release(context.Context, string, InterfaceTarget) (bool, error)
}
type IngressLeases interface {
	Acquire(context.Context, string, IngressTarget) (bool, error)
	Release(context.Context, string, IngressTarget) (bool, error)
}

// ForwardingResources composes the three durable EC2 lease journals. Instances
// are shared by all attachments in a scope under one elected controller.
type ForwardingResources struct {
	Scope   Scope
	Routes  RouteLeases
	Checks  CheckLeases
	Ingress IngressLeases
}

func (f ForwardingResources) validate(b ForwardingBinding) error {
	if f.Routes == nil || f.Checks == nil || f.Ingress == nil || b.Lease == "" || b.Scope != f.Scope || b.Gateway.InterfaceID == "" || b.Gateway.InstanceID == "" || len(b.Routes) == 0 || len(b.Ingress) == 0 {
		return fmt.Errorf("complete persisted forwarding binding and matching scope required")
	}
	for _, route := range b.Routes {
		if route.InterfaceID != b.Gateway.InterfaceID || route.InstanceID != b.Gateway.InstanceID {
			return fmt.Errorf("return route belongs to a different gateway")
		}
	}
	return nil
}

// Ensure retains partial progress for idempotent retry or explicit retirement.
// It never rolls back shared settings merely because another API is unavailable.
func (f ForwardingResources) Ensure(ctx context.Context, b ForwardingBinding) (bool, error) {
	if err := f.validate(b); err != nil {
		return false, err
	}
	if ok, err := f.Checks.Acquire(ctx, b.Lease, b.Gateway); err != nil || !ok {
		return false, err
	}
	for _, rule := range b.Ingress {
		if ok, err := f.Ingress.Acquire(ctx, b.Lease, rule); err != nil || !ok {
			return false, err
		}
	}
	for _, route := range b.Routes {
		if ok, err := f.Routes.Acquire(ctx, b.Lease, route); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// Release is called only after consumer withdrawal is acknowledged. Attempt all
// independent removals even if one fails, but preserve gateway forwarding until
// every ingress and route lease has retired. Underlying journals protect peers.
func (f ForwardingResources) Release(ctx context.Context, b ForwardingBinding) (bool, error) {
	if err := f.validate(b); err != nil {
		return false, err
	}
	ready := true
	var failures []error
	for _, rule := range b.Ingress {
		ok, err := f.Ingress.Release(ctx, b.Lease, rule)
		ready = ready && ok && err == nil
		if err != nil {
			failures = append(failures, err)
		}
	}
	for _, route := range b.Routes {
		ok, err := f.Routes.Release(ctx, b.Lease, route)
		ready = ready && ok && err == nil
		if err != nil {
			failures = append(failures, err)
		}
	}
	if !ready {
		return false, errors.Join(failures...)
	}
	return f.Checks.Release(ctx, b.Lease, b.Gateway)
}

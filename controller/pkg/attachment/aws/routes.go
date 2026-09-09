// Package aws implements EC2 forwarding resources behind provider-independent
// attachment leases. It does not select guest bootstrap or CNI implementations.
package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sync"
)

// API is implemented by a credential-scoped EC2 transport. The harness CLI
// already implements this interface; no setup credentials enter a journal.
type API interface {
	Call(context.Context, string, string, map[string]any) (json.RawMessage, error)
}

type Scope struct {
	Account, Region, VPCID, SubnetID, RouteTableID, OwnerTag, OwnerValue string
}

type RouteTarget struct {
	Destination netip.Prefix `json:"destination"`
	InterfaceID string       `json:"interfaceID"`
	InstanceID  string       `json:"instanceID"`
}

type RouteRecord struct {
	Key      string          `json:"key"`
	Target   RouteTarget     `json:"target"`
	Leases   map[string]bool `json:"leases"`
	Phase    string          `json:"phase"`
	Version  string          `json:"-"`
	Retiring bool            `json:"-"`
}

// RouteJournal uses compare-and-swap writes. A Deleting record cannot be
// reacquired until deletion has been observed and Absent has been persisted.
type RouteJournal interface {
	Load(context.Context, string) (*RouteRecord, error)
	Save(context.Context, *RouteRecord, *RouteRecord) error
}

// Routes is shared by all worker leases in one ownership scope. The hosting
// controller must have one elected active writer for that scope.
type Routes struct {
	mu      sync.Mutex
	API     API
	Journal RouteJournal
	Scope   Scope
}

type routeObservation struct {
	DestinationCidrBlock, NetworkInterfaceId, State, Origin string
}

func (r *Routes) key(t RouteTarget) string {
	return r.Scope.Account + "/" + r.Scope.Region + "/" + r.Scope.RouteTableID + "/" + t.Destination.String()
}

func (r *Routes) observe(ctx context.Context, t RouteTarget, acquire bool) (*routeObservation, error) {
	s := r.Scope
	if r.API == nil || r.Journal == nil || s.Account == "" || s.Region == "" || s.VPCID == "" || s.SubnetID == "" || s.RouteTableID == "" || s.OwnerTag == "" || s.OwnerValue == "" {
		return nil, fmt.Errorf("EC2 route adapter requires a complete owned scope")
	}
	if !t.Destination.IsValid() || !t.Destination.Addr().Is4() || t.Destination.Bits() != 32 || t.InterfaceID == "" || t.InstanceID == "" {
		return nil, fmt.Errorf("an explicit IPv4 host and bound gateway interface are required")
	}
	raw, err := r.API.Call(ctx, "ec2", "describe-route-tables", map[string]any{"RouteTableIds": []string{s.RouteTableID}})
	if err != nil {
		return nil, err
	}
	var tables struct {
		RouteTables []struct {
			RouteTableId, VpcId, OwnerId string
			Tags                         []struct{ Key, Value string }
			Routes                       []routeObservation
		}
	}
	if err := json.Unmarshal(raw, &tables); err != nil {
		return nil, err
	}
	if len(tables.RouteTables) != 1 {
		return nil, fmt.Errorf("route table identity is ambiguous")
	}
	table := tables.RouteTables[0]
	owned := false
	for _, tag := range table.Tags {
		if tag.Key == s.OwnerTag && tag.Value == s.OwnerValue {
			owned = true
		}
	}
	if !owned || table.RouteTableId != s.RouteTableID || table.VpcId != s.VPCID || table.OwnerId != s.Account {
		return nil, fmt.Errorf("route table ownership mismatch")
	}
	var found *routeObservation
	for _, v := range table.Routes {
		if v.Origin == "CreateRouteTable" {
			if p, e := netip.ParsePrefix(v.DestinationCidrBlock); e == nil && p.Contains(t.Destination.Addr()) {
				return nil, fmt.Errorf("host route would override VPC local routing")
			}
		}
		if v.DestinationCidrBlock == t.Destination.String() {
			copy := v
			found = &copy
		}
	}
	{
		raw, err = r.API.Call(ctx, "ec2", "describe-network-interfaces", map[string]any{"Filters": []map[string]any{{"Name": "network-interface-id", "Values": []string{t.InterfaceID}}}})
		if err != nil {
			return nil, err
		}
		var interfaces struct {
			NetworkInterfaces []struct {
				NetworkInterfaceId, VpcId, SubnetId, OwnerId, Status string
				Attachment                                           struct{ InstanceId, Status string }
			}
		}
		if err := json.Unmarshal(raw, &interfaces); err != nil {
			return nil, err
		}
		if len(interfaces.NetworkInterfaces) == 0 && !acquire {
			return found, nil
		}
		if len(interfaces.NetworkInterfaces) != 1 {
			return nil, fmt.Errorf("gateway interface identity is ambiguous")
		}
		i := interfaces.NetworkInterfaces[0]
		if i.NetworkInterfaceId != t.InterfaceID || i.VpcId != s.VPCID || i.SubnetId != s.SubnetID || i.OwnerId != s.Account || (i.Attachment.InstanceId != "" && i.Attachment.InstanceId != t.InstanceID) || (acquire && (i.Status != "in-use" || i.Attachment.InstanceId != t.InstanceID || i.Attachment.Status != "attached")) {
			return nil, fmt.Errorf("gateway interface binding changed")
		}
	}
	return found, nil
}

func copyRecord(old *RouteRecord) *RouteRecord {
	next := *old
	next.Leases = map[string]bool{}
	for k, v := range old.Leases {
		next.Leases[k] = v
	}
	return &next
}

// Acquire persists intent before CreateRoute. A matching route without a
// journal is foreign, even if adopting it would make this test pass.
func (r *Routes) Acquire(ctx context.Context, lease string, t RouteTarget) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lease == "" {
		return false, fmt.Errorf("empty attachment lease")
	}
	observed, err := r.observe(ctx, t, true)
	if err != nil {
		return false, err
	}
	old, err := r.Journal.Load(ctx, r.key(t))
	if err != nil {
		return false, err
	}
	if old != nil && old.Retiring {
		return false, fmt.Errorf("route journal is retiring")
	}
	if old != nil && (old.Key != r.key(t) || (old.Phase != "Absent" && old.Target != t)) {
		return false, fmt.Errorf("route is journaled for another gateway")
	}
	if old != nil && old.Phase == "Deleting" {
		return false, nil
	}
	if old == nil || old.Phase == "Absent" {
		if observed != nil {
			return false, fmt.Errorf("refusing to adopt a pre-existing route")
		}
		next := &RouteRecord{Key: r.key(t), Target: t, Leases: map[string]bool{lease: true}, Phase: "Creating"}
		if err := r.Journal.Save(ctx, old, next); err != nil {
			return false, err
		}
		// Reload the persisted CAS token; all following retries use this intent.
		old, err = r.Journal.Load(ctx, r.key(t))
		if err != nil {
			return false, err
		}
	} else {
		if old.Phase != "Creating" && old.Phase != "Active" {
			return false, fmt.Errorf("unknown route journal phase")
		}
		if observed != nil && observed.NetworkInterfaceId != t.InterfaceID {
			return false, fmt.Errorf("route target changed outside this lease")
		}
		if !old.Leases[lease] {
			next := copyRecord(old)
			next.Leases[lease] = true
			if err := r.Journal.Save(ctx, old, next); err != nil {
				return false, err
			}
			old, err = r.Journal.Load(ctx, r.key(t))
			if err != nil {
				return false, err
			}
		}
	}
	if observed == nil {
		if old.Phase != "Creating" {
			next := copyRecord(old)
			next.Phase = "Creating"
			if err := r.Journal.Save(ctx, old, next); err != nil {
				return false, err
			}
			old, err = r.Journal.Load(ctx, r.key(t))
			if err != nil {
				return false, err
			}
		}
		_, createErr := r.API.Call(ctx, "ec2", "create-route", map[string]any{"RouteTableId": r.Scope.RouteTableID, "DestinationCidrBlock": t.Destination.String(), "NetworkInterfaceId": t.InterfaceID})
		observed, err = r.observe(ctx, t, true)
		if err != nil {
			return false, err
		}
		if observed == nil {
			return false, createErr
		}
		// CreateRoute can succeed before a client timeout. Only read-back of the
		// exact persisted target resolves an uncertain response.
	}
	if observed.NetworkInterfaceId != t.InterfaceID {
		return false, fmt.Errorf("route target differs from persisted intent")
	}
	if observed.State != "active" {
		return false, nil
	}
	if old.Phase != "Active" {
		next := copyRecord(old)
		next.Phase = "Active"
		if err := r.Journal.Save(ctx, old, next); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Release must be called only after consumer withdrawal. It does not require
// the old gateway instance to exist: terminated gateways still leave routes
// needing cleanup. The table and exact route target are revalidated instead.
func (r *Routes) Release(ctx context.Context, lease string, t RouteTarget) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lease == "" {
		return false, fmt.Errorf("empty attachment lease")
	}
	observed, err := r.observe(ctx, t, false)
	if err != nil {
		return false, err
	}
	old, err := r.Journal.Load(ctx, r.key(t))
	if err != nil {
		return false, err
	}
	if old == nil {
		return true, nil
	}
	if old.Key != r.key(t) || old.Target != t {
		return false, fmt.Errorf("route journal identity mismatch")
	}
	if old.Phase == "Absent" {
		return true, nil
	}
	if old.Phase != "Creating" && old.Phase != "Active" && old.Phase != "Deleting" {
		return false, fmt.Errorf("unknown route journal phase")
	}
	if old.Phase != "Deleting" {
		if !old.Leases[lease] {
			return true, nil
		}
		next := copyRecord(old)
		delete(next.Leases, lease)
		if len(next.Leases) == 0 {
			next.Phase = "Deleting"
		}
		if err := r.Journal.Save(ctx, old, next); err != nil {
			return false, err
		}
		if len(next.Leases) > 0 {
			return true, nil
		}
		old, err = r.Journal.Load(ctx, r.key(t))
		if err != nil {
			return false, err
		}
	}
	if observed != nil && observed.NetworkInterfaceId != t.InterfaceID {
		return false, fmt.Errorf("refusing to delete a route now targeting another interface")
	}
	if observed != nil {
		_, deleteErr := r.API.Call(ctx, "ec2", "delete-route", map[string]any{"RouteTableId": r.Scope.RouteTableID, "DestinationCidrBlock": t.Destination.String()})
		observed, err = r.observe(ctx, t, false)
		if err != nil {
			return false, err
		}
		if observed != nil {
			return false, deleteErr
		}
	}
	next := copyRecord(old)
	next.Phase = "Absent"
	if err := r.Journal.Save(ctx, old, next); err != nil {
		return false, err
	}
	return true, nil
}

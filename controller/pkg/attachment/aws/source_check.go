package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

type InterfaceTarget struct {
	InterfaceID string `json:"interfaceID"`
	InstanceID  string `json:"instanceID"`
}

type CheckRecord struct {
	Key      string          `json:"key"`
	Target   InterfaceTarget `json:"target"`
	Original bool            `json:"original"`
	Leases   map[string]bool `json:"leases"`
	Phase    string          `json:"phase"`
	Version  string          `json:"-"`
	Retiring bool            `json:"-"`
}

type CheckJournal interface {
	Load(context.Context, string) (*CheckRecord, error)
	Save(context.Context, *CheckRecord, *CheckRecord) error
}

// SourceChecks manages the specific gateway ENI, not an implicitly selected
// primary interface. One elected controller shares this instance across leases.
type SourceChecks struct {
	mu      sync.Mutex
	API     API
	Journal CheckJournal
	Scope   Scope
}

func (c *SourceChecks) key(t InterfaceTarget) string {
	return c.Scope.Account + "/" + c.Scope.Region + "/" + t.InterfaceID + "/source-dest-check"
}

func (c *SourceChecks) observe(ctx context.Context, t InterfaceTarget, acquire bool) (*bool, error) {
	s := c.Scope
	if c.API == nil || c.Journal == nil || s.Account == "" || s.Region == "" || s.VPCID == "" || s.SubnetID == "" || s.OwnerTag == "" || s.OwnerValue == "" || t.InstanceID == "" || t.InterfaceID == "" {
		return nil, fmt.Errorf("source-check adapter requires owned scope and bound interface")
	}
	raw, err := c.API.Call(ctx, "ec2", "describe-network-interfaces", map[string]any{"Filters": []map[string]any{{"Name": "network-interface-id", "Values": []string{t.InterfaceID}}}})
	if err != nil {
		return nil, err
	}
	var response struct {
		NetworkInterfaces []struct {
			NetworkInterfaceId, OwnerId, VpcId, SubnetId, Status string
			SourceDestCheck                                      *bool
			Attachment                                           struct{ InstanceId, Status string }
			TagSet                                               []struct{ Key, Value string }
		}
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if len(response.NetworkInterfaces) == 0 && !acquire {
		return nil, nil
	}
	if len(response.NetworkInterfaces) != 1 {
		return nil, fmt.Errorf("gateway interface is absent or ambiguous")
	}
	i := response.NetworkInterfaces[0]
	owned := false
	for _, tag := range i.TagSet {
		if tag.Key == s.OwnerTag && tag.Value == s.OwnerValue {
			owned = true
		}
	}
	if !owned || i.NetworkInterfaceId != t.InterfaceID || i.OwnerId != s.Account || i.VpcId != s.VPCID || i.SubnetId != s.SubnetID || i.SourceDestCheck == nil {
		return nil, fmt.Errorf("gateway interface ownership or source-check observation is incomplete")
	}
	if (i.Attachment.InstanceId != "" && i.Attachment.InstanceId != t.InstanceID) || (acquire && (i.Status != "in-use" || i.Attachment.Status != "attached" || i.Attachment.InstanceId != t.InstanceID)) {
		return nil, fmt.Errorf("gateway interface binding changed")
	}
	return i.SourceDestCheck, nil
}

func copyCheck(old *CheckRecord) *CheckRecord {
	next := *old
	next.Leases = map[string]bool{}
	for k, v := range old.Leases {
		next.Leases[k] = v
	}
	return &next
}

func (c *SourceChecks) Acquire(ctx context.Context, lease string, t InterfaceTarget) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if lease == "" {
		return false, fmt.Errorf("empty attachment lease")
	}
	observed, err := c.observe(ctx, t, true)
	if err != nil {
		return false, err
	}
	old, err := c.Journal.Load(ctx, c.key(t))
	if err != nil {
		return false, err
	}
	if old != nil && (old.Key != c.key(t) || (old.Phase != "Restored" && old.Target != t)) {
		return false, fmt.Errorf("source-check journal belongs to another gateway")
	}
	if old != nil && old.Retiring {
		return false, fmt.Errorf("source-check journal is retiring")
	}
	if old != nil && old.Phase == "Restoring" {
		return false, nil
	}
	if old == nil || old.Phase == "Restored" {
		next := &CheckRecord{Key: c.key(t), Target: t, Original: *observed, Leases: map[string]bool{lease: true}, Phase: "Disabling"}
		if err := c.Journal.Save(ctx, old, next); err != nil {
			return false, err
		}
	} else {
		if old.Phase != "Disabling" && old.Phase != "Active" {
			return false, fmt.Errorf("unknown source-check journal phase")
		}
		next := copyCheck(old)
		next.Leases[lease] = true
		if *observed {
			next.Phase = "Disabling"
		}
		if !old.Leases[lease] || next.Phase != old.Phase {
			if err := c.Journal.Save(ctx, old, next); err != nil {
				return false, err
			}
		}
	}
	old, err = c.Journal.Load(ctx, c.key(t))
	if err != nil {
		return false, err
	}
	if old == nil {
		return false, fmt.Errorf("source-check intent disappeared")
	}
	if *observed {
		_, modifyErr := c.API.Call(ctx, "ec2", "modify-network-interface-attribute", map[string]any{"NetworkInterfaceId": t.InterfaceID, "SourceDestCheck": map[string]any{"Value": false}})
		observed, err = c.observe(ctx, t, true)
		if err != nil {
			return false, err
		}
		if *observed {
			return false, modifyErr
		}
	}
	if old.Phase != "Active" {
		next := copyCheck(old)
		next.Phase = "Active"
		if err := c.Journal.Save(ctx, old, next); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Release restores the recorded original setting only after the final lease.
// A gateway that already had checks disabled is never enabled by this adapter.
func (c *SourceChecks) Release(ctx context.Context, lease string, t InterfaceTarget) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if lease == "" {
		return false, fmt.Errorf("empty attachment lease")
	}
	observed, err := c.observe(ctx, t, false)
	if err != nil {
		return false, err
	}
	old, err := c.Journal.Load(ctx, c.key(t))
	if err != nil {
		return false, err
	}
	if old == nil {
		return true, nil
	}
	if old.Key != c.key(t) || old.Target != t {
		return false, fmt.Errorf("source-check journal identity mismatch")
	}
	if old.Phase == "Restored" {
		return true, nil
	}
	if old.Phase != "Disabling" && old.Phase != "Active" && old.Phase != "Restoring" {
		return false, fmt.Errorf("unknown source-check journal phase")
	}
	if old.Phase != "Restoring" {
		if !old.Leases[lease] {
			return true, nil
		}
		next := copyCheck(old)
		delete(next.Leases, lease)
		if len(next.Leases) == 0 {
			next.Phase = "Restoring"
		}
		if err := c.Journal.Save(ctx, old, next); err != nil {
			return false, err
		}
		if len(next.Leases) > 0 {
			return true, nil
		}
		old, err = c.Journal.Load(ctx, c.key(t))
		if err != nil {
			return false, err
		}
		if old == nil {
			return false, fmt.Errorf("source-check restoration intent disappeared")
		}
	}
	if observed != nil && old.Original && !*observed {
		_, modifyErr := c.API.Call(ctx, "ec2", "modify-network-interface-attribute", map[string]any{"NetworkInterfaceId": t.InterfaceID, "SourceDestCheck": map[string]any{"Value": true}})
		observed, err = c.observe(ctx, t, false)
		if err != nil {
			return false, err
		}
		if observed != nil && !*observed {
			return false, modifyErr
		}
	}
	next := copyCheck(old)
	next.Phase = "Restored"
	if err := c.Journal.Save(ctx, old, next); err != nil {
		return false, err
	}
	return true, nil
}

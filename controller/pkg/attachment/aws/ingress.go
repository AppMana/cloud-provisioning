package aws

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/netip"
	"sync"
)

const ingressOwnerTag = "cloud-provisioning.appmana.com/ingress-owner"

type IngressTarget struct {
	GroupID  string       `json:"groupID"`
	Source   netip.Prefix `json:"source"`
	Port     uint16       `json:"port"`
	Protocol string       `json:"protocol,omitempty"`
}
type IngressRecord struct {
	Key      string          `json:"key"`
	Target   IngressTarget   `json:"target"`
	Token    string          `json:"token"`
	RuleID   string          `json:"ruleID"`
	Leases   map[string]bool `json:"leases"`
	Phase    string          `json:"phase"`
	Version  string          `json:"-"`
	Retiring bool            `json:"-"`
}
type IngressJournal interface {
	Load(context.Context, string) (*IngressRecord, error)
	Save(context.Context, *IngressRecord, *IngressRecord) error
}

// Ingress shares exact TCP/UDP host rules across attachments. One elected writer
// owns each scope. A random, persisted creation tag resolves lost responses
// without adopting pre-existing permissions or deleting replacement rules.
type Ingress struct {
	mu      sync.Mutex
	API     API
	Journal IngressJournal
	Scope   Scope
}
type ingressRule struct {
	SecurityGroupRuleId, GroupId, GroupOwnerId, IpProtocol, CidrIpv4 string
	IsEgress                                                         bool
	FromPort, ToPort                                                 int
	Tags                                                             []struct{ Key, Value string }
}

func (t IngressTarget) protocol() string {
	if t.Protocol == "" {
		return "udp"
	}
	return t.Protocol
}

func (r *Ingress) key(t IngressTarget) string {
	return fmt.Sprintf("%s/%s/%s/%s/%d/%s", r.Scope.Account, r.Scope.Region, t.GroupID, t.protocol(), t.Port, t.Source)
}
func (r *Ingress) observe(ctx context.Context, t IngressTarget, acquire bool) ([]ingressRule, error) {
	s := r.Scope
	if r.API == nil || r.Journal == nil || s.Account == "" || s.Region == "" || s.VPCID == "" || s.OwnerTag == "" || s.OwnerValue == "" || s.OwnerTag == ingressOwnerTag || t.GroupID == "" || !t.Source.IsValid() || !t.Source.Addr().Is4() || t.Source.Bits() != 32 || !t.Source.Addr().IsGlobalUnicast() || t.Source.Addr().IsLoopback() || t.Port == 0 || (t.protocol() != "udp" && t.protocol() != "tcp") {
		return nil, fmt.Errorf("ingress requires owned scope and an explicit IPv4 TCP/UDP host permission")
	}
	raw, err := r.API.Call(ctx, "ec2", "describe-security-groups", map[string]any{"Filters": []map[string]any{{"Name": "group-id", "Values": []string{t.GroupID}}}})
	if err != nil {
		return nil, err
	}
	var groups struct {
		SecurityGroups []struct {
			GroupId, OwnerId, VpcId string
			Tags                    []struct{ Key, Value string }
		}
	}
	if err = json.Unmarshal(raw, &groups); err != nil {
		return nil, err
	}
	if len(groups.SecurityGroups) == 0 && !acquire {
		return nil, nil
	}
	if len(groups.SecurityGroups) != 1 {
		return nil, fmt.Errorf("security group absent or ambiguous")
	}
	g := groups.SecurityGroups[0]
	owned := false
	for _, tag := range g.Tags {
		if tag.Key == s.OwnerTag && tag.Value == s.OwnerValue {
			owned = true
		}
	}
	if !owned || g.GroupId != t.GroupID || g.OwnerId != s.Account || g.VpcId != s.VPCID {
		return nil, fmt.Errorf("security group ownership mismatch")
	}
	var rules []ingressRule
	token := ""
	seen := map[string]bool{}
	for {
		input := map[string]any{"Filters": []map[string]any{{"Name": "group-id", "Values": []string{t.GroupID}}}}
		if token != "" {
			input["NextToken"] = token
		}
		raw, err = r.API.Call(ctx, "ec2", "describe-security-group-rules", input)
		if err != nil {
			return nil, err
		}
		var page struct {
			SecurityGroupRules []ingressRule
			NextToken          string
		}
		if err = json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		for _, rule := range page.SecurityGroupRules {
			if rule.GroupId != t.GroupID || rule.GroupOwnerId != s.Account {
				return nil, fmt.Errorf("foreign rule in group observation")
			}
			rules = append(rules, rule)
		}
		token = page.NextToken
		if token == "" {
			break
		}
		if seen[token] {
			return nil, fmt.Errorf("repeated security group pagination token")
		}
		seen[token] = true
	}
	return rules, nil
}
func matchesIngress(rule ingressRule, t IngressTarget) bool {
	protocol := t.protocol()
	number := "17"
	if protocol == "tcp" {
		number = "6"
	}
	return !rule.IsEgress && (rule.IpProtocol == protocol || rule.IpProtocol == number) && rule.FromPort == int(t.Port) && rule.ToPort == int(t.Port) && rule.CidrIpv4 == t.Source.String()
}
func (r *Ingress) ownedRule(rules []ingressRule, record *IngressRecord) (*ingressRule, error) {
	if record.Token == "" {
		return nil, fmt.Errorf("missing ingress ownership token")
	}
	var found *ingressRule
	for _, rule := range rules {
		token := ""
		scopeOwned := false
		for _, tag := range rule.Tags {
			if tag.Key == ingressOwnerTag {
				token = tag.Value
			}
			if tag.Key == r.Scope.OwnerTag && tag.Value == r.Scope.OwnerValue {
				scopeOwned = true
			}
		}
		exact := matchesIngress(rule, record.Target)
		if token == record.Token || (record.RuleID != "" && rule.SecurityGroupRuleId == record.RuleID) || exact {
			if !exact || !scopeOwned || token != record.Token || rule.SecurityGroupRuleId == "" || (record.RuleID != "" && rule.SecurityGroupRuleId != record.RuleID) {
				return nil, fmt.Errorf("ingress rule changed or belongs to another owner")
			}
			if found != nil {
				return nil, fmt.Errorf("ambiguous ingress ownership")
			}
			copy := rule
			found = &copy
		}
	}
	return found, nil
}
func copyIngress(old *IngressRecord) *IngressRecord {
	next := *old
	next.Leases = map[string]bool{}
	for k, v := range old.Leases {
		next.Leases[k] = v
	}
	return &next
}
func (r *Ingress) Acquire(ctx context.Context, lease string, t IngressTarget) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lease == "" {
		return false, fmt.Errorf("empty attachment lease")
	}
	rules, err := r.observe(ctx, t, true)
	if err != nil {
		return false, err
	}
	old, err := r.Journal.Load(ctx, r.key(t))
	if err != nil {
		return false, err
	}
	if old != nil && (old.Key != r.key(t) || old.Target != t) {
		return false, fmt.Errorf("ingress journal identity mismatch")
	}
	if old != nil && old.Retiring {
		return false, fmt.Errorf("ingress journal is retiring")
	}
	if old != nil && old.Phase == "Deleting" {
		return false, nil
	}
	if old == nil || old.Phase == "Absent" {
		for _, rule := range rules {
			if matchesIngress(rule, t) {
				return false, fmt.Errorf("refusing existing unowned ingress permission")
			}
		}
		token := make([]byte, 16)
		if _, err = rand.Read(token); err != nil {
			return false, err
		}
		next := &IngressRecord{Key: r.key(t), Target: t, Token: fmt.Sprintf("%x", token), Leases: map[string]bool{lease: true}, Phase: "Creating"}
		if err = r.Journal.Save(ctx, old, next); err != nil {
			return false, err
		}
	} else {
		if old.Phase != "Creating" && old.Phase != "Active" {
			return false, fmt.Errorf("unknown ingress phase")
		}
		if _, err = r.ownedRule(rules, old); err != nil {
			return false, err
		}
		if !old.Leases[lease] {
			next := copyIngress(old)
			next.Leases[lease] = true
			if err = r.Journal.Save(ctx, old, next); err != nil {
				return false, err
			}
		}
	}
	old, err = r.Journal.Load(ctx, r.key(t))
	if err != nil {
		return false, err
	}
	if old == nil || old.Token == "" {
		return false, fmt.Errorf("ingress creation intent missing")
	}
	rule, err := r.ownedRule(rules, old)
	if err != nil {
		return false, err
	}
	if rule == nil {
		if old.Phase != "Creating" || old.RuleID != "" {
			next := copyIngress(old)
			next.Phase = "Creating"
			next.RuleID = ""
			if err = r.Journal.Save(ctx, old, next); err != nil {
				return false, err
			}
			old, err = r.Journal.Load(ctx, r.key(t))
			if err != nil {
				return false, err
			}
			if old == nil {
				return false, fmt.Errorf("ingress repair intent missing")
			}
		}
		_, createErr := r.API.Call(ctx, "ec2", "authorize-security-group-ingress", map[string]any{"GroupId": t.GroupID, "IpPermissions": []map[string]any{{"IpProtocol": t.protocol(), "FromPort": int(t.Port), "ToPort": int(t.Port), "IpRanges": []map[string]any{{"CidrIp": t.Source.String()}}}}, "TagSpecifications": []map[string]any{{"ResourceType": "security-group-rule", "Tags": []map[string]any{{"Key": ingressOwnerTag, "Value": old.Token}, {"Key": r.Scope.OwnerTag, "Value": r.Scope.OwnerValue}}}}})
		rules, err = r.observe(ctx, t, true)
		if err != nil {
			return false, err
		}
		rule, err = r.ownedRule(rules, old)
		if err != nil {
			return false, err
		}
		if rule == nil {
			return false, createErr
		}
	}
	if old.Phase != "Active" || old.RuleID != rule.SecurityGroupRuleId {
		next := copyIngress(old)
		next.Phase = "Active"
		next.RuleID = rule.SecurityGroupRuleId
		if err = r.Journal.Save(ctx, old, next); err != nil {
			return false, err
		}
	}
	return true, nil
}
func (r *Ingress) Release(ctx context.Context, lease string, t IngressTarget) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lease == "" {
		return false, fmt.Errorf("empty attachment lease")
	}
	rules, err := r.observe(ctx, t, false)
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
		return false, fmt.Errorf("ingress journal identity mismatch")
	}
	if old.Phase == "Absent" {
		return true, nil
	}
	if old.Phase != "Creating" && old.Phase != "Active" && old.Phase != "Deleting" {
		return false, fmt.Errorf("unknown ingress phase")
	}
	if old.Phase != "Deleting" {
		if !old.Leases[lease] {
			return true, nil
		}
		next := copyIngress(old)
		delete(next.Leases, lease)
		if len(next.Leases) == 0 {
			next.Phase = "Deleting"
		}
		if err = r.Journal.Save(ctx, old, next); err != nil {
			return false, err
		}
		if len(next.Leases) > 0 {
			return true, nil
		}
		old, err = r.Journal.Load(ctx, r.key(t))
		if err != nil {
			return false, err
		}
		if old == nil {
			return false, fmt.Errorf("ingress deletion intent missing")
		}
	}
	rule, err := r.ownedRule(rules, old)
	if err != nil {
		return false, err
	}
	if rule != nil {
		_, deleteErr := r.API.Call(ctx, "ec2", "revoke-security-group-ingress", map[string]any{"GroupId": t.GroupID, "SecurityGroupRuleIds": []string{rule.SecurityGroupRuleId}})
		rules, err = r.observe(ctx, t, false)
		if err != nil {
			return false, err
		}
		rule, err = r.ownedRule(rules, old)
		if err != nil {
			return false, err
		}
		if rule != nil {
			return false, deleteErr
		}
	}
	next := copyIngress(old)
	next.Phase = "Absent"
	if err = r.Journal.Save(ctx, old, next); err != nil {
		return false, err
	}
	return true, nil
}

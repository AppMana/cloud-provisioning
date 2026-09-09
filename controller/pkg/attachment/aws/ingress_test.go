package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"
)

type ingressAPI struct {
	rules            []ingressRule
	journal          IngressJournal
	key              string
	creates, deletes int
	loseResponse     bool
	missing          bool
}

func (e *ingressAPI) Call(ctx context.Context, _, op string, in map[string]any) (json.RawMessage, error) {
	var result any
	switch op {
	case "describe-security-groups":
		if e.missing {
			return json.Marshal(map[string]any{"SecurityGroups": []any{}})
		}
		result = map[string]any{"SecurityGroups": []any{map[string]any{"GroupId": "sg-test", "OwnerId": "account", "VpcId": "vpc-owned", "Tags": []map[string]string{{"Key": "run", "Value": "run"}}}}}
	case "describe-security-group-rules":
		result = map[string]any{"SecurityGroupRules": e.rules}
	case "authorize-security-group-ingress":
		r, err := e.journal.Load(ctx, e.key)
		if err != nil || r == nil || r.Phase != "Creating" {
			return nil, fmt.Errorf("no create intent")
		}
		e.creates++
		rule := ingressRule{SecurityGroupRuleId: fmt.Sprintf("sgr-%d", e.creates), GroupId: "sg-test", GroupOwnerId: "account", IpProtocol: in["IpPermissions"].([]map[string]any)[0]["IpProtocol"].(string), CidrIpv4: r.Target.Source.String(), FromPort: int(r.Target.Port), ToPort: int(r.Target.Port)}
		for _, tag := range in["TagSpecifications"].([]map[string]any)[0]["Tags"].([]map[string]any) {
			rule.Tags = append(rule.Tags, struct{ Key, Value string }{tag["Key"].(string), tag["Value"].(string)})
		}
		e.rules = append(e.rules, rule)
		if e.loseResponse {
			return nil, fmt.Errorf("lost response")
		}
		result = map[string]any{}
	case "revoke-security-group-ingress":
		r, err := e.journal.Load(ctx, e.key)
		if err != nil || r == nil || r.Phase != "Deleting" {
			return nil, fmt.Errorf("no delete intent")
		}
		ids := in["SecurityGroupRuleIds"].([]string)
		if len(ids) != 1 {
			return nil, fmt.Errorf("expected one rule ID")
		}
		var kept []ingressRule
		for _, rule := range e.rules {
			if rule.SecurityGroupRuleId != ids[0] {
				kept = append(kept, rule)
			}
		}
		e.rules = kept
		e.deletes++
		if e.loseResponse {
			return nil, fmt.Errorf("lost response")
		}
		result = map[string]any{}
	default:
		return nil, fmt.Errorf("unexpected operation %s", op)
	}
	return json.Marshal(result)
}

type ingressFailJournal struct {
	IngressJournal
	failPhase string
}

func (j *ingressFailJournal) Save(ctx context.Context, old, next *IngressRecord) error {
	if next.Phase == j.failPhase {
		j.failPhase = ""
		return fmt.Errorf("journal unavailable")
	}
	return j.IngressJournal.Save(ctx, old, next)
}
func ingressFixture(t *testing.T) (*Ingress, *ingressAPI, *ingressFailJournal, IngressTarget) {
	r, _, j, _ := routeFixture(t)
	base := j.RouteJournal.(ConfigMapJournal)
	journal := &ingressFailJournal{IngressJournal: ConfigMapIngressJournal{Client: base.Client, Namespace: base.Namespace}}
	api := &ingressAPI{journal: journal}
	adapter := &Ingress{API: api, Journal: journal, Scope: r.Scope}
	target := IngressTarget{GroupID: "sg-test", Source: netip.MustParsePrefix("10.10.0.11/32"), Port: 4789}
	api.key = adapter.key(target)
	return adapter, api, journal, target
}
func TestIngressSharedLifecycle(t *testing.T) {
	ctx := context.Background()
	r, e, _, target := ingressFixture(t)
	e.loseResponse = true
	for _, lease := range []string{"2022", "2025"} {
		if ok, err := r.Acquire(ctx, lease, target); err != nil || !ok {
			t.Fatalf("acquire %v %v", ok, err)
		}
	}
	if e.creates != 1 {
		t.Fatal("duplicate permission")
	}
	if ok, err := r.Release(ctx, "2022", target); err != nil || !ok || len(e.rules) != 1 {
		t.Fatal("first release removed shared rule")
	}
	if ok, err := r.Release(ctx, "2025", target); err != nil || !ok || len(e.rules) != 0 || e.deletes != 1 {
		t.Fatal("final release failed")
	}
	if ok, err := r.Acquire(ctx, "new", target); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := r.Release(ctx, "2025", target); err != nil || !ok || len(e.rules) != 1 {
		t.Fatal("stale lease removed replacement")
	}
}
func TestIngressRejectsChangedRules(t *testing.T) {
	for _, kind := range []string{"replacement-id", "port", "tag"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			r, e, _, target := ingressFixture(t)
			if ok, err := r.Acquire(ctx, "worker", target); err != nil || !ok {
				t.Fatal(err)
			}
			switch kind {
			case "replacement-id":
				e.rules[0].SecurityGroupRuleId = "sgr-other"
			case "port":
				e.rules[0].ToPort++
			case "tag":
				e.rules[0].Tags = nil
			}
			if _, err := r.Release(ctx, "worker", target); err == nil || e.deletes != 0 {
				t.Fatal("deleted changed rule")
			}
		})
	}
}
func TestIngressPersistenceRestartAndRepair(t *testing.T) {
	ctx := context.Background()
	r, e, j, target := ingressFixture(t)
	j.failPhase = "Creating"
	if _, err := r.Acquire(ctx, "worker", target); err == nil || e.creates != 0 {
		t.Fatal("mutation before intent")
	}
	j.failPhase = "Active"
	if ok, err := r.Acquire(ctx, "worker", target); err == nil || ok || e.creates != 1 {
		t.Fatal("expected uncommitted create")
	}
	r = &Ingress{API: e, Journal: j, Scope: r.Scope}
	if ok, err := r.Acquire(ctx, "worker", target); err != nil || !ok || e.creates != 1 {
		t.Fatalf("restart %v %v", ok, err)
	}
	e.rules = nil
	if ok, err := r.Acquire(ctx, "worker", target); err != nil || !ok || e.creates != 2 {
		t.Fatalf("repair %v %v", ok, err)
	}
	if ok, err := r.Release(ctx, "worker", target); err != nil || !ok {
		t.Fatal(err)
	}
}
func TestIngressRejectsPreexistingRule(t *testing.T) {
	r, e, _, target := ingressFixture(t)
	e.rules = []ingressRule{{SecurityGroupRuleId: "sgr-external", GroupId: "sg-test", GroupOwnerId: "account", IpProtocol: "17", CidrIpv4: target.Source.String(), FromPort: 4789, ToPort: 4789}}
	if _, err := r.Acquire(context.Background(), "worker", target); err == nil || e.creates != 0 {
		t.Fatal("adopted preexisting permission")
	}
}

func TestIngressDeletedGroupRetirement(t *testing.T) {
	ctx := context.Background()
	r, e, _, target := ingressFixture(t)
	if ok, err := r.Acquire(ctx, "worker", target); err != nil || !ok {
		t.Fatal(err)
	}
	e.missing = true
	if ok, err := r.Acquire(ctx, "new", target); err == nil || ok {
		t.Fatal("acquired absent group")
	}
	if ok, err := r.Release(ctx, "worker", target); err != nil || !ok || e.deletes != 0 {
		t.Fatal("deleted group could not retire without mutation")
	}
}

func TestTCPAPIPermissionHasSeparateLeaseAndExactRemoval(t *testing.T) {
	r, e, _, target := ingressFixture(t)
	target.Port = 6443
	udpKey := r.key(target)
	target.Protocol = "tcp"
	if r.key(target) == udpKey {
		t.Fatal("TCP and UDP leases collide")
	}
	e.key = r.key(target)
	e.rules = []ingressRule{{SecurityGroupRuleId: "sgr-unowned-udp", GroupId: "sg-test", GroupOwnerId: "account", IpProtocol: "udp", CidrIpv4: target.Source.String(), FromPort: 6443, ToPort: 6443}}
	e.loseResponse = true
	if ok, err := r.Acquire(context.Background(), "api", target); err != nil || !ok {
		t.Fatalf("TCP acquire: %v %v", ok, err)
	}
	if e.rules[1].IpProtocol != "tcp" {
		t.Fatal("created wrong protocol")
	}
	numeric := e.rules[1]
	numeric.IpProtocol = "6"
	if !matchesIngress(numeric, target) {
		t.Fatal("numeric EC2 TCP protocol rejected")
	}
	if ok, err := r.Release(context.Background(), "api", target); err != nil || !ok {
		t.Fatalf("TCP release: %v %v", ok, err)
	}
	if len(e.rules) != 1 || e.rules[0].SecurityGroupRuleId != "sgr-unowned-udp" {
		t.Fatal("removed unrelated UDP rule")
	}
	target.Protocol = "-1"
	if _, err := r.Acquire(context.Background(), "all", target); err == nil {
		t.Fatal("unrestricted protocol accepted")
	}
}

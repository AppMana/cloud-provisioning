package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Field shapes come from the real cldt-windows-v2 EC2 route table and ENI.
type ec2 struct {
	sourceCheck, missingCheck, foreignInterface, loseCheckResponse bool
	modifications                                                  []bool
	target, instance                                               string
	missing                                                        bool
	creates, deletes                                               int
	foreignTable, detached, timeoutCreate                          bool
}

func (e *ec2) Call(_ context.Context, _ string, op string, in map[string]any) (json.RawMessage, error) {
	var result any
	switch op {
	case "describe-route-tables":
		value := "run"
		if e.foreignTable {
			value = "another-run"
		}
		routes := []map[string]string{{"DestinationCidrBlock": "172.29.0.0/24", "GatewayId": "local", "Origin": "CreateRouteTable", "State": "active"}}
		if e.target != "" {
			routes = append(routes, map[string]string{"DestinationCidrBlock": "10.10.0.11/32", "NetworkInterfaceId": e.target, "Origin": "CreateRoute", "State": "active"})
		}
		result = map[string]any{"RouteTables": []any{map[string]any{"RouteTableId": "rtb-owned", "VpcId": "vpc-owned", "OwnerId": "account", "Tags": []map[string]string{{"Key": "run", "Value": value}}, "Routes": routes}}}
	case "describe-network-interfaces":
		tag := "run"
		if e.foreignInterface {
			tag = "foreign"
		}
		var check *bool
		if !e.missingCheck {
			check = &e.sourceCheck
		}

		if e.missing {
			return json.Marshal(map[string]any{"NetworkInterfaces": []any{}})
		}
		instance := e.instance
		if instance == "" {
			instance = "i-gateway"
		}
		status := "attached"
		if e.detached {
			status = "detached"
		}
		result = map[string]any{"NetworkInterfaces": []any{map[string]any{"NetworkInterfaceId": "eni-gateway", "VpcId": "vpc-owned", "SubnetId": "subnet-owned", "OwnerId": "account", "TagSet": []map[string]string{{"Key": "run", "Value": tag}}, "SourceDestCheck": check, "Status": "in-use", "Attachment": map[string]string{"InstanceId": instance, "Status": status}}}}
	case "modify-network-interface-attribute":
		e.sourceCheck = in["SourceDestCheck"].(map[string]any)["Value"].(bool)
		e.modifications = append(e.modifications, e.sourceCheck)
		if e.loseCheckResponse {
			return nil, fmt.Errorf("response lost after modifying check")
		}
		result = map[string]any{}
	case "create-route":
		e.creates++
		e.target = in["NetworkInterfaceId"].(string)
		if e.timeoutCreate {
			return nil, fmt.Errorf("response lost after EC2 create")
		}
		result = map[string]any{"Return": true}
	case "delete-route":
		e.deletes++
		e.target = ""
		result = map[string]any{}
	default:
		return nil, fmt.Errorf("unexpected operation %s", op)
	}
	return json.Marshal(result)
}

type failureJournal struct {
	RouteJournal
	fail bool
}

func (j *failureJournal) Save(ctx context.Context, old, next *RouteRecord) error {
	if j.fail {
		j.fail = false
		return fmt.Errorf("journal write failed")
	}
	return j.RouteJournal.Save(ctx, old, next)
}

func routeFixture(t *testing.T) (*Routes, *ec2, *failureJournal, RouteTarget) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	j := &failureJournal{RouteJournal: ConfigMapJournal{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "test"}}
	e := &ec2{}
	r := &Routes{API: e, Journal: j, Scope: Scope{Account: "account", Region: "region", VPCID: "vpc-owned", SubnetID: "subnet-owned", RouteTableID: "rtb-owned", OwnerTag: "run", OwnerValue: "run"}}
	return r, e, j, RouteTarget{Destination: netip.MustParsePrefix("10.10.0.11/32"), InterfaceID: "eni-gateway", InstanceID: "i-gateway"}
}

func TestSharedRouteSurvivesFirstWorkerRemoval(t *testing.T) {
	ctx := context.Background()
	r, e, _, target := routeFixture(t)
	for _, lease := range []string{"windows-2022", "windows-2025"} {
		if ready, err := r.Acquire(ctx, lease, target); err != nil || !ready {
			t.Fatalf("%v %v", ready, err)
		}
	}
	if e.creates != 1 {
		t.Fatal("shared route created twice")
	}
	if done, err := r.Release(ctx, "windows-2022", target); err != nil || !done || e.deletes != 0 {
		t.Fatal("first worker removal deleted shared route")
	}
	// The gateway may be gone by the time the final worker retires.
	e.missing = true
	if done, err := r.Release(ctx, "windows-2025", target); err != nil || !done || e.deletes != 1 {
		t.Fatalf("final release failed: %v %v", done, err)
	}
	e.missing = false
	if ready, err := r.Acquire(ctx, "replacement", target); err != nil || !ready {
		t.Fatalf("reattach %v %v", ready, err)
	}
	if done, err := r.Release(ctx, "windows-2025", target); err != nil || !done || e.deletes != 1 {
		t.Fatal("stale old release removed replacement route")
	}
}

func TestRouteCreationRequiresIntentAndResolvesUncertainResponse(t *testing.T) {
	ctx := context.Background()
	r, e, j, target := routeFixture(t)
	j.fail = true
	if _, err := r.Acquire(ctx, "worker", target); err == nil || e.creates != 0 {
		t.Fatal("cloud mutated before durable journal")
	}
	e.timeoutCreate = true
	if ready, err := r.Acquire(ctx, "worker", target); err != nil || !ready {
		t.Fatal("exact read-back did not resolve uncertain create")
	}
	e.target = "" // Drift: the same lease restores an absent owned route.
	if ready, err := r.Acquire(ctx, "worker", target); err != nil || !ready || e.creates != 2 {
		t.Fatal("owned route was not repaired")
	}
}

func TestForeignResourcesAreNeverAdoptedOrDeleted(t *testing.T) {
	ctx := context.Background()
	r, e, _, target := routeFixture(t)
	e.target = target.InterfaceID
	if _, err := r.Acquire(ctx, "worker", target); err == nil || e.creates != 0 {
		t.Fatal("foreign matching route adopted")
	}
	e.target = ""
	e.foreignTable = true
	if _, err := r.Acquire(ctx, "worker", target); err == nil || e.creates != 0 {
		t.Fatal("foreign table mutated")
	}
	e.foreignTable = false
	e.detached = true
	if _, err := r.Acquire(ctx, "worker", target); err == nil {
		t.Fatal("detached gateway accepted")
	}
	e.detached = false
	if ready, err := r.Acquire(ctx, "worker", target); err != nil || !ready {
		t.Fatal(err)
	}
	e.target = "eni-external-change"
	if _, err := r.Release(ctx, "worker", target); err == nil || e.deletes != 0 {
		t.Fatal("externally changed route deleted")
	}
	if ready, err := r.Acquire(ctx, "new-worker", target); err != nil || ready {
		t.Fatal("reacquired route while deletion is pending")
	}
}

func TestHostRouteCannotOverrideVpcLocalNetwork(t *testing.T) {
	ctx := context.Background()
	r, e, _, target := routeFixture(t)
	target.Destination = netip.MustParsePrefix("172.29.0.99/32")
	if _, err := r.Acquire(ctx, "worker", target); err == nil || e.creates != 0 {
		t.Fatal("overrode VPC local route")
	}
}

func TestReleaseRejectsInterfaceReattachedToAnotherMachine(t *testing.T) {
	ctx := context.Background()
	r, e, _, target := routeFixture(t)
	if ready, err := r.Acquire(ctx, "worker", target); err != nil || !ready {
		t.Fatal(err)
	}
	e.instance = "i-unrelated"
	if _, err := r.Release(ctx, "worker", target); err == nil || e.deletes != 0 {
		t.Fatal("deleted route after gateway ENI was reassigned")
	}
}

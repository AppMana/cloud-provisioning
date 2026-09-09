// awsroutecheck validates shared return-route ownership on an isolated AWS run.
// It creates one scoped host route and releases both leases before returning.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	provider "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	rig "github.com/appmana/cloud-provisioning/harness/e2e/rig/aws"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"
)

type options struct {
	work, plans, apiServer, bastion, destination, output string
	recover                                              bool
}

type journal struct{ path string }

func (j journal) Load(_ context.Context, key string) (*provider.RouteRecord, error) {
	raw, e := os.ReadFile(j.path)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var r provider.RouteRecord
	if e = json.Unmarshal(raw, &r); e != nil {
		return nil, e
	}
	if r.Key != key {
		return nil, fmt.Errorf("journal key mismatch")
	}
	r.Version = fmt.Sprintf("%x", sha256.Sum256(raw))
	return &r, nil
}
func (j journal) Save(ctx context.Context, old, next *provider.RouteRecord) error {
	current, e := j.Load(ctx, next.Key)
	if e != nil {
		return e
	}
	if (old == nil) != (current == nil) || (old != nil && old.Version != current.Version) {
		return fmt.Errorf("journal conflict")
	}
	raw, e := json.Marshal(next)
	if e != nil {
		return e
	}
	tmp, e := os.CreateTemp(filepath.Dir(j.path), "route-")
	if e != nil {
		return e
	}
	defer os.Remove(tmp.Name())
	if _, e = tmp.Write(raw); e != nil {
		tmp.Close()
		return e
	}
	if e = tmp.Sync(); e != nil {
		tmp.Close()
		return e
	}
	if e = tmp.Close(); e != nil {
		return e
	}
	return os.Rename(tmp.Name(), j.path)
}

type observedAPI struct {
	cli     *rig.CLI
	journal journal
	ops     []string
}

func (a *observedAPI) Call(ctx context.Context, s, o string, in map[string]any) (json.RawMessage, error) {
	if o == "create-route" || o == "delete-route" {
		raw, e := os.ReadFile(a.journal.path)
		if e != nil {
			return nil, fmt.Errorf("no durable mutation intent")
		}
		var r provider.RouteRecord
		if e = json.Unmarshal(raw, &r); e != nil {
			return nil, e
		}
		want := "Creating"
		if o == "delete-route" {
			want = "Deleting"
		}
		if r.Phase != want {
			return nil, fmt.Errorf("mutation phase %s, want %s", r.Phase, want)
		}
		a.ops = append(a.ops, o)
	}
	return a.cli.Call(ctx, s, o, in)
}
func main() {
	var o options
	flag.StringVar(&o.work, "work-dir", "", "private AWS run directory")
	flag.StringVar(&o.plans, "plans", "", "observed gateway plan results JSON")
	flag.StringVar(&o.apiServer, "api-server", "", "isolated site API server")
	flag.StringVar(&o.bastion, "bastion", "clab-cldt-bastion", "site bastion container")
	flag.StringVar(&o.destination, "destination", "", "one observed CNI return host prefix")
	flag.StringVar(&o.output, "output-dir", "", "new private evidence directory")
	flag.BoolVar(&o.recover, "recover", false, "release persisted leases from an interrupted check")
	flag.Parse()
	if e := run(o); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run(o options) (err error) {
	if o.work == "" || o.output == "" || (!o.recover && (o.plans == "" || o.apiServer == "" || o.destination == "")) {
		return fmt.Errorf("work-dir, plans, api-server, destination and output-dir are required")
	}
	work := o.work
	destination, e := netip.ParsePrefix(o.destination)
	if !o.recover && (e != nil || !destination.Addr().Is4() || destination.Bits() != 32) {
		return fmt.Errorf("destination must be an IPv4 host prefix")
	}
	lock, e := os.OpenFile(filepath.Join(work, "gateway-route-check.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return fmt.Errorf("another route check owns this run")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	raw, e := os.ReadFile(work + "/resources.json")
	if e != nil {
		return e
	}
	var s struct {
		Account, Region, VPCID, SubnetID, RouteTableID, RunID string
		CleanedUp                                             bool
	}
	if e = json.Unmarshal(raw, &s); e != nil {
		return e
	}
	if s.CleanedUp {
		return fmt.Errorf("run is cleaned up")
	}
	if o.recover {
		return recoverRoutes(ctx, o, provider.Scope{Account: s.Account, Region: s.Region, VPCID: s.VPCID, SubnetID: s.SubnetID, RouteTableID: s.RouteTableID, OwnerTag: "cloud-provisioning-test", OwnerValue: s.RunID})
	}
	var plans struct {
		Results []struct{ Plan attachment.GatewayPlan }
	}
	raw, e = os.ReadFile(o.plans)
	if e != nil {
		return e
	}
	if e = json.Unmarshal(raw, &plans); e != nil {
		return e
	}
	if len(plans.Results) != 2 {
		return fmt.Errorf("need two worker plans")
	}
	type machine struct {
		Metadata struct{ UID string }
		Spec     struct{ ProviderID, ClusterName string }
		Status   struct {
			Phase   string
			NodeRef struct{ Name string }
		}
	}
	raw, e = exec.CommandContext(ctx, "docker", "exec", o.bastion, "kubectl", "--server="+o.apiServer, "-n", "cloud-provisioning", "get", "machines", "-o", "json").Output()
	if e != nil {
		return fmt.Errorf("CAPI observation failed")
	}
	var machines struct{ Items []machine }
	if e = json.Unmarshal(raw, &machines); e != nil {
		return e
	}
	if plans.Results[0].Plan.Worker.UID == plans.Results[1].Plan.Worker.UID {
		return fmt.Errorf("two distinct worker identities are required")
	}
	for _, row := range plans.Results {
		if row.Plan.Gateway != plans.Results[0].Plan.Gateway {
			return fmt.Errorf("workers must share the same gateway identity")
		}
		permitted := false
		for _, host := range row.Plan.ReturnHosts {
			if host == destination {
				permitted = true
			}
		}
		if !permitted {
			return fmt.Errorf("destination was not observed for both workers")
		}
		for _, identity := range []attachment.Machine{row.Plan.Worker, row.Plan.Gateway} {
			var found *machine
			for i := range machines.Items {
				if machines.Items[i].Metadata.UID == identity.UID {
					found = &machines.Items[i]
				}
			}
			if found == nil || found.Spec.ProviderID != identity.ProviderID || found.Spec.ClusterName != s.RunID || found.Status.Phase != "Running" {
				return fmt.Errorf("CAPI identity changed")
			}
			raw, e = exec.CommandContext(ctx, "docker", "exec", o.bastion, "kubectl", "--server="+o.apiServer, "get", "node", found.Status.NodeRef.Name, "-o", "json").Output()
			if e != nil {
				return fmt.Errorf("Node observation failed")
			}
			var node struct {
				Metadata struct{ UID string }
				Spec     struct{ ProviderID string }
			}
			if e = json.Unmarshal(raw, &node); e != nil {
				return e
			}
			if node.Metadata.UID != identity.NodeUID || node.Spec.ProviderID != identity.ProviderID {
				return fmt.Errorf("Node identity changed")
			}
		}
	}
	dir := o.output
	if e = os.Mkdir(dir, 0700); e != nil {
		return fmt.Errorf("new validation directory required: %w", e)
	}
	j := journal{dir + "/route.json"}
	api := &observedAPI{cli: &rig.CLI{Region: s.Region, SessionPath: work + "/gateway-session.json"}, journal: j}
	r := &provider.Routes{API: api, Journal: j, Scope: provider.Scope{Account: s.Account, Region: s.Region, VPCID: s.VPCID, SubnetID: s.SubnetID, RouteTableID: s.RouteTableID, OwnerTag: "cloud-provisioning-test", OwnerValue: s.RunID}}
	gateway := plans.Results[0].Plan.Gateway
	t := provider.RouteTarget{Destination: destination, InterfaceID: gateway.InterfaceID, InstanceID: gateway.ProviderID[strings.LastIndex(gateway.ProviderID, "/")+1:]}
	leases := []string{plans.Results[0].Plan.Worker.UID + "/" + rand.Text(), plans.Results[1].Plan.Worker.UID + "/" + rand.Text()}
	meta, _ := json.Marshal(map[string]any{"leases": leases, "target": t, "scope": r.Scope})
	if e = os.WriteFile(dir+"/inputs.json", meta, 0600); e != nil {
		return e
	}
	routes := func() ([]string, error) {
		raw, e := api.cli.Call(ctx, "ec2", "describe-route-tables", map[string]any{"RouteTableIds": []string{s.RouteTableID}})
		if e != nil {
			return nil, e
		}
		var v struct {
			RouteTables []struct{ Routes []json.RawMessage }
		}
		if e = json.Unmarshal(raw, &v); e != nil {
			return nil, e
		}
		if len(v.RouteTables) != 1 {
			return nil, fmt.Errorf("ambiguous table")
		}
		var out []string
		for _, entry := range v.RouteTables[0].Routes {
			var obj any
			if e = json.Unmarshal(entry, &obj); e != nil {
				return nil, e
			}
			canonical, _ := json.Marshal(obj)
			out = append(out, string(canonical))
		}
		sort.Strings(out)
		return out, nil
	}
	before, e := routes()
	if e != nil {
		return e
	}
	report := map[string]any{"scope": "Real EC2 shared route leases for two live CAPI worker identities; no CNI publication or end-to-end gateway claim", "startedAt": time.Now().UTC(), "target": t, "routeTableID": s.RouteTableID}
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), time.Minute)
		defer c()
		failures := releaseLeases(cleanup, r, leases, t)
		report["cleanupErrors"] = failures
		report["mutations"] = api.ops
		if err != nil {
			report["error"] = err.Error()
		}
		if len(failures) > 0 {
			err = fmt.Errorf("route cleanup needs recovery: %v", failures)
		}
		report["finishedAt"] = time.Now().UTC()
		data, _ := json.MarshalIndent(report, "", "  ")
		if writeErr := os.WriteFile(dir+"/evidence.json", data, 0600); writeErr != nil {
			err = errors.Join(err, writeErr)
		}
	}()
	for _, lease := range leases {
		if ok, e := r.Acquire(ctx, lease, t); e != nil || !ok {
			return fmt.Errorf("acquire incomplete: %v", e)
		}
	}
	if ok, e := r.Release(ctx, leases[0], t); e != nil || !ok {
		return fmt.Errorf("first release: %v", e)
	}
	shared, e := routes()
	if e != nil {
		return e
	}
	present := false
	for _, raw := range shared {
		var v struct{ DestinationCidrBlock, NetworkInterfaceId string }
		if e = json.Unmarshal([]byte(raw), &v); e != nil {
			return e
		}
		if v.DestinationCidrBlock == t.Destination.String() && v.NetworkInterfaceId == t.InterfaceID {
			present = true
		}
	}
	if !present || len(shared) != len(before)+1 {
		return fmt.Errorf("shared route disappeared")
	}
	report["firstRemovalPreservedSharedRoute"] = true
	if ok, e := r.Release(ctx, leases[1], t); e != nil || !ok {
		return fmt.Errorf("last release: %v", e)
	}
	after, e := routes()
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("original routing not restored")
	}
	report["originalRoutesRestored"] = true
	if !reflect.DeepEqual(api.ops, []string{"create-route", "delete-route"}) {
		return fmt.Errorf("unexpected route mutations")
	}
	report["oneCreateOneDelete"] = true
	fmt.Println("Real EC2 route shared by both worker leases, retained after first release, removed after final release; original routes restored")
	return nil
}

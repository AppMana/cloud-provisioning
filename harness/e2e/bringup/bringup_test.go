package bringup

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

// The isolation proof must not be able to pass by finding nothing.
//
// This is the exact shape of the bug it exists to catch: a check that
// enumerates what to test, gets an empty list, tests none of it, and
// reports success. The site being unreachable from a cloud is the
// property every result on this lab rests on, so a vacuous pass here
// makes the whole campaign meaningless.
func TestAVacuousIsolationProofIsAFailure(t *testing.T) {
	h := &fakeHost{
		// Every reachability probe fails, which is what isolation
		// looks like — so the only thing that can go wrong is the
		// enumeration.
		addrOutput: "",
	}
	err := Prove(context.Background(), lab.Default(), HostProber{Host: h})
	if err == nil {
		t.Fatal("the proof passed while reading no addresses at all")
	}
	if !strings.Contains(err.Error(), "tested nothing") {
		t.Errorf("the failure does not say it tested nothing: %v", err)
	}
}

// A cloud reaching any address a site node holds fails the proof, and
// the message has to name the address, because the whole point is
// that it is one nobody thought to check.
func TestAPathTheSegmentsDoNotExplainFailsTheProof(t *testing.T) {
	h := &fakeHost{
		addrOutput: "2: eth1    inet 10.10.0.10/24 scope global eth1\\       valid_lft forever",
		// The management network's shape: a cloud node can reach a
		// site node at an address the topology never gave it.
		reachable: map[string]bool{"remote1->10.10.0.10": true},
	}
	err := Prove(context.Background(), lab.Default(), HostProber{Host: h})
	if err == nil {
		t.Fatal("a reachable site address passed the proof")
	}
	if !strings.Contains(err.Error(), "10.10.0.10") {
		t.Errorf("the failure does not name the address: %v", err)
	}
	if !strings.Contains(err.Error(), "meaningless") {
		t.Errorf("the failure does not say what it costs: %v", err)
	}
}

// The site's router masquerades outward and admits nothing inward;
// the cloud edges forward both ways. That difference is the entire
// distinction between a private site and a public cloud, so it is
// asserted rather than trusted to a list of node names.
func TestTheSiteFiltersInwardAndTheCloudsDoNot(t *testing.T) {
	r := &fakeRig{}
	if err := policy(context.Background(), lab.Default(), r); err != nil {
		t.Fatal(err)
	}

	router := r.commandsFor("router")
	if !containsCommand(router, "-P", "FORWARD", "DROP") {
		t.Error("the site's router does not default to dropping forwarded traffic, so it forwards inward too")
	}
	if !containsCommand(router, "MASQUERADE") {
		t.Error("the site's router does not masquerade, so a reply has no way back")
	}
	if !containsCommand(router, "ESTABLISHED,RELATED") {
		t.Error("the site's router admits nothing inward at all, not even answers to what left")
	}

	for _, edge := range []string{"edge-a", "edge-b"} {
		cmds := r.commandsFor(edge)
		if !containsCommand(cmds, "-P", "FORWARD", "ACCEPT") {
			t.Errorf("%s does not forward both ways, so a public address is not reachable", edge)
		}
		if containsCommand(cmds, "MASQUERADE") {
			t.Errorf("%s translates, so a remote's address is not the address it is reached at", edge)
		}
	}
}

// Every node gets a default route, and it is its own segment's edge.
// A node routed through another segment's edge would still reach
// things, which is why this is checked rather than inferred from the
// lab working.
func TestEveryNodeLeavesByItsOwnEdge(t *testing.T) {
	r := &fakeRig{}
	if err := routes(context.Background(), lab.Default(), r); err != nil {
		t.Fatal(err)
	}
	for node, via := range map[string]string{
		"cp":      lab.SitePrefix + ".1",
		"w1":      lab.SitePrefix + ".1",
		"remote1": lab.CloudAPrefix + ".1",
		"remote2": lab.CloudBPrefix + ".1",
	} {
		if !containsCommand(r.commandsFor(node), "default", "via", via) {
			t.Errorf("%s does not leave by %s: %v", node, via, r.commandsFor(node))
		}
	}
}

type fakeHost struct {
	addrOutput string
	reachable  map[string]bool
}

func (f *fakeHost) Run(ctx context.Context, argv ...string) ([]byte, error) { return nil, nil }

func (f *fakeHost) InNamespace(ctx context.Context, node string, argv ...string) ([]byte, error) {
	switch argv[0] {
	case "ip":
		return []byte(f.addrOutput), nil
	case "ping":
		target := argv[len(argv)-1]
		// A working lab: everything reaches everything except an
		// address at the site, which is private. So the only probes
		// that fail by default are the isolation ones, and a test can
		// open a specific hole to assert the proof notices.
		if strings.HasPrefix(target, lab.SitePrefix+".") && !f.reachable[node+"->"+target] {
			return nil, errUnreachable
		}
		return nil, nil
	default:
		return nil, errUnreachable
	}
}

var errUnreachable = &unreachable{}

type unreachable struct{}

func (*unreachable) Error() string { return "unreachable" }

func containsCommand(cmds [][]string, words ...string) bool {
	for _, cmd := range cmds {
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, strings.Join(words, " ")) {
			return true
		}
	}
	return false
}

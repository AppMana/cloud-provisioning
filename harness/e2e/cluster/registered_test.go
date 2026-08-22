package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
)

type fakeLister struct {
	calls int
	sets  [][]string
	err   error
}

func (f *fakeLister) Nodes(context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	i := f.calls
	f.calls++
	if i >= len(f.sets) {
		i = len(f.sets) - 1
	}
	return f.sets[i], nil
}

// The defect this exists to end: a site that is still registering
// must not be reported as the site.
func TestASiteStillRegisteringIsNotTheSite(t *testing.T) {
	topo := lab.Default()
	k := &fakeLister{sets: [][]string{{"cp", "cp2", "cp3"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := WaitRegistered(ctx, k, topo); err == nil {
		t.Fatal("a five-node site reported three nodes and was accepted")
	} else if !strings.Contains(err.Error(), "w1") || !strings.Contains(err.Error(), "w2") {
		t.Errorf("the failure does not name what is missing: %v", err)
	}
}

// A node that is merely slow is waited for, not failed.
func TestALateNodeIsWaitedFor(t *testing.T) {
	topo := lab.Default()
	k := &fakeLister{sets: [][]string{
		{"cp"},
		{"cp", "cp2", "cp3"},
		{"cp", "cp2", "cp3", "w1", "w2"},
	}}

	got, err := WaitRegistered(context.Background(), k, topo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("returned %v", got)
	}
	if k.calls < 3 {
		t.Errorf("gave up after %d reads", k.calls)
	}
}

// A remote that has joined is a node the site never had, and must
// not make the site look wrong.
func TestExtraNodesAreNotAFailure(t *testing.T) {
	topo := lab.Default()
	k := &fakeLister{sets: [][]string{{"cp", "cp2", "cp3", "w1", "w2", "remote1"}}}
	if _, err := WaitRegistered(context.Background(), k, topo); err != nil {
		t.Fatal(err)
	}
}

func TestAnUnreadableClusterIsAFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	k := &fakeLister{err: errors.New("no member answered")}
	if _, err := WaitRegistered(ctx, k, topoOrDie(t)); err == nil {
		t.Fatal("an unreadable cluster was accepted")
	}
}

func topoOrDie(t *testing.T) lab.Topology { t.Helper(); return lab.Default() }

// A remote that was claimed and bootstrapped has to be there before
// anything measures the lab. A matrix taken while it is still joining
// tests a lab without the thing under test, and passes.
func TestAClaimedRemoteMustHaveJoined(t *testing.T) {
	topo := lab.Default()
	site := &fakeLister{sets: [][]string{{"cp", "cp2", "cp3", "w1", "w2"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := WaitRegistered(ctx, site, topo, "remote1"); err == nil {
		t.Fatal("a matrix was allowed to run before the claimed remote had joined")
	} else if !strings.Contains(err.Error(), "remote1") {
		t.Errorf("the failure does not name the remote: %v", err)
	}

	joined := &fakeLister{sets: [][]string{{"cp", "cp2", "cp3", "w1", "w2", "remote1"}}}
	if _, err := WaitRegistered(context.Background(), joined, topo, "remote1"); err != nil {
		t.Fatal(err)
	}
}

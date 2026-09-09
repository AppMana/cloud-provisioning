package outage

import (
	"bytes"
	"context"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
)

// The single-endpoint k0s/kube-router cut retained direct remote paths while
// the API marked both remotes NotReady. The original external URL redirected
// to DNS and failed: those failures must remain required, never be excused as
// part of the allowed site isolation.
func TestObservedSoleEndpointIsolation(t *testing.T) {
	for _, externalBroken := range []bool{false, true} {
		p := &observedIsolation{externalBroken: externalBroken, byIP: map[string]string{}}
		hooks := &fakeProber{onCut: func() { p.down = true }, onRestore: func() { p.down = false }}
		d := Deps{Rig: &fakeRig{prober: hooks}, Prober: p, Options: check.Options{Port: 8080, ExternalURL: check.ExternalURL}, Converge: 100 * time.Millisecond, Down: time.Millisecond}
		for i, name := range []string{"cp", "cp2", "cp3", "w1", "w2", "remote1", "remote2"} {
			ip := []string{"10.244.0.8", "10.244.1.6", "10.244.4.6", "10.244.2.11", "10.244.3.12", "10.244.8.4", "10.244.9.4"}[i]
			d.Targets = append(d.Targets, check.Target{Node: name, PodIP: ip, ServiceIP: ip})
			p.byIP[ip] = name
		}
		res := Run(context.Background(), Row{Victim: "w1", Mode: Cut, SiteIsolated: []string{"remote1", "remote2"}}, d)
		if res.OK() == externalBroken {
			t.Fatalf("external broken=%v: %s", externalBroken, res)
		}
		if res.Survivors == nil || res.Survivors.NotRequired() != 50 {
			t.Fatalf("unexpected isolation accounting: %+v", res.Survivors)
		}
		if externalBroken && len(res.Survivors.Failures()) != 2 {
			t.Fatalf("external failures were hidden: %v", res.Survivors.Failures())
		}
		if p.down {
			t.Fatal("failed survivor check left the endpoint down")
		}
	}
}

type observedIsolation struct {
	down, externalBroken bool
	byIP                 map[string]string
}

func remote(name string) bool { return slices.Contains([]string{"remote1", "remote2"}, name) }
func (p *observedIsolation) HTTPGet(_ context.Context, from, target string) ([]byte, error) {
	u, _ := url.Parse(target)
	to := p.byIP[u.Hostname()]
	if p.down && ((to != "" && remote(from) != remote(to)) || (to == "" && remote(from) && p.externalBroken)) {
		return nil, errDown
	}
	if u.Path == "/big" {
		return bytes.Repeat([]byte("x"), check.TransferBytes), nil
	}
	return []byte("ok"), nil
}
func (p *observedIsolation) Resolve(_ context.Context, from, _ string) error {
	if p.down && remote(from) {
		return errDown
	}
	return nil
}

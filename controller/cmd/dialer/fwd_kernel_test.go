package main

import (
	"os"
	"testing"

	"github.com/google/nftables"
)

// Exercises the real kernel path: run under a network namespace with a
// device present. Skipped unless asked for, since it writes nftables.
func TestForwardingPathAgainstTheKernel(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}
	if err := ensureForwardingPath("dummy0", 1420); err != nil {
		t.Fatalf("ensureForwardingPath: %v", err)
	}
}

// Tunnel routes and source permissions come from the mesh record. Routing
// sessions must not cross the tunnel: endpoint transitions can interrupt their
// withdrawals and leave stale routes behind.
func TestTheTunnelRefusesBGPAgainstTheKernel(t *testing.T) {
	if os.Getenv("CLDT_NETNS") != "1" {
		t.Skip("set CLDT_NETNS=1 inside a namespace")
	}
	if err := ensureForwardingPath("dummy0", 1420); err != nil {
		t.Fatalf("ensureForwardingPath: %v", err)
	}
	c, err := nftables.New()
	if err != nil {
		t.Fatalf("opening nftables: %v", err)
	}
	defer c.CloseLasting()
	tables, err := c.ListTables()
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	var bgpTable *nftables.Table
	for _, table := range tables {
		if table.Name == "cldt-bgp-dummy0" {
			bgpTable = table
		}
	}
	if bgpTable == nil {
		t.Fatal("no BGP boundary table: routing sessions can cross the tunnel and re-announce stale claims after every transition")
	}
	chains, err := c.ListChainsOfTableFamily(nftables.TableFamilyINet)
	if err != nil {
		t.Fatalf("listing chains: %v", err)
	}
	hooks := map[string]bool{}
	for _, chain := range chains {
		if chain.Table.Name == bgpTable.Name && chain.Hooknum != nil {
			hooks[chain.Name] = true
			rules, err := c.GetRules(bgpTable, chain)
			if err != nil {
				t.Fatalf("listing rules in %s: %v", chain.Name, err)
			}
			if len(rules) == 0 {
				t.Errorf("chain %s has no rules, so its hook filters nothing", chain.Name)
			}
		}
	}
	for _, want := range []string{"input", "output", "forward"} {
		if !hooks[want] {
			t.Errorf("no %s chain: one direction of a session across the tunnel is still open", want)
		}
	}
}

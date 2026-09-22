package lab

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/srl-labs/containerlab/core"
	clablinks "github.com/srl-labs/containerlab/links"
	"gopkg.in/yaml.v2"
)

// Preserve node/cable parity with the legacy CLI fixture while both exist.
// Only this compatibility test reads YAML; the new topology is built in code.
func TestTheGeneratedTopologyMatchesTheOneBashDeploys(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "clab", "topo.clab.yml"))
	if os.IsNotExist(err) {
		t.Skip("legacy CLI fixture has been retired")
	}
	if err != nil {
		t.Fatal(err)
	}
	var legacy core.Config
	if err := yaml.Unmarshal(body, &legacy); err != nil {
		t.Fatal(err)
	}
	generated, err := Default().ContainerlabConfig(Container)
	if err != nil {
		t.Fatal(err)
	}
	nodeNames := func(config *core.Config) []string {
		var names []string
		for name := range config.Topology.Nodes {
			names = append(names, name)
		}
		sort.Strings(names)
		return names
	}
	linkPairs := func(config *core.Config) []string {
		var pairs []string
		for _, link := range config.Topology.Links {
			var endpoints []string
			switch raw := link.Link.(type) {
			case *clablinks.LinkBriefRaw:
				endpoints = append(endpoints, raw.Endpoints...)
			case *clablinks.LinkVEthRaw:
				for _, endpoint := range raw.Endpoints {
					endpoints = append(endpoints, endpoint.Node+":"+endpoint.Iface)
				}
			default:
				t.Fatalf("unexpected link type %T", raw)
			}
			sort.Strings(endpoints)
			pairs = append(pairs, strings.Join(endpoints, " <-> "))
		}
		sort.Strings(pairs)
		return pairs
	}
	if got, want := nodeNames(generated), nodeNames(&legacy); len(got) == 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("nodes differ: %v versus %v", got, want)
	}
	if got, want := linkPairs(generated), linkPairs(&legacy); len(got) == 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("links differ: %v versus %v", got, want)
	}
}

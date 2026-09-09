package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// NodeLister reads the cluster's registered nodes.
type NodeLister interface {
	Nodes(ctx context.Context) ([]string, error)
}

// RegistrationTimeout is how long a site's nodes have to appear.
//
// A kubelet registers after its node has started, and on a machine
// that gap is a boot and a first-ever image pull wide.
const RegistrationTimeout = 10 * time.Minute

// WaitRegistered blocks until every node the topology puts at the
// site has registered, and fails if any is missing. Its variadic
//
// A floor, not a reading. This was a single list and a printed line,
// and it reported "registered: [cp cp2 cp3]" for a five-node site as
// though that were the answer: the two workers had started and were
// still registering. Everything downstream then measured the short
// list — the readiness gate waited only for the nodes that happened
// to have appeared, so a site missing two of its five passed both.
//
// On containers a kubelet registers fast enough that one read caught
// them all, which is how a missing gate survived a green matrix.
// also names nodes beyond the site that must be there by now: a
// remote that has been claimed and bootstrapped is one of them, and a
// matrix measured before it joined is a matrix that never tested it.
func WaitRegistered(ctx context.Context, k NodeLister, t lab.Topology, also ...string) ([]string, error) {
	want := make([]string, 0, len(t.Nodes))
	for _, n := range SiteNodes(t) {
		want = append(want, n.Name)
	}
	want = append(want, also...)
	if len(want) == 0 {
		return nil, fmt.Errorf("the topology puts no node at the site, so this proved nothing")
	}
	sort.Strings(want)

	var got []string
	err := wait.Until(ctx, RegistrationTimeout,
		"not every node the topology puts at the site registered", func(ctx context.Context) error {
			var err error
			got, err = k.Nodes(ctx)
			if err != nil {
				return err
			}
			if missing := absent(want, got); len(missing) > 0 {
				return fmt.Errorf("%s never registered (have %s)",
					strings.Join(missing, ", "), strings.Join(got, ", "))
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return got, nil
}

// absent is every wanted name the cluster does not carry. More nodes
// than wanted is not an error here: a remote joins later and is a
// node the site never had.
func absent(want, got []string) []string {
	have := map[string]bool{}
	for _, n := range got {
		have[n] = true
	}
	var missing []string
	for _, n := range want {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return missing
}

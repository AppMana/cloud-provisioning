// Package outage takes a node away and makes three claims about
// everyone else.
//
// A row is a sequence, and each step exists because the one before it
// would otherwise mean something weaker:
//
//   - The baseline is green before anything breaks. A failure
//     measured on a broken baseline names the wrong culprit.
//   - The survivors converge among themselves while the victim is
//     down. Losing one node may cost that node's pods and nothing
//     else.
//   - The victim returns and the whole cluster is green again, with
//     nothing reinstalled, restarted or forgiven.
//
// Two ways down, and they are different claims. A cut takes the
// network and leaves the machine running against it: a pulled cable,
// which from everywhere else is silence. A reboot takes the machine,
// ungracefully, and gives it back only what a platform provides — a
// NIC, an address, a gateway — so the tunnel, the routes and the
// membership must be rebuilt by what the node itself runs at boot.
//
// reboot-remote is the invariant's proof: the host unit has to raise
// the tunnel from its cached peer list while the cluster is
// unreachable, because the cluster is on the far side of the tunnel
// it is raising.
package outage

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/check"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

// Mode is how a node goes away.
type Mode string

const (
	// Cut pulls the cable and leaves the machine running.
	Cut Mode = "cut"
	// Reboot kills the machine and brings it back.
	Reboot Mode = "reboot"
)

// Row is one outage.
type Row struct {
	// SiteIsolated lists remotes when their sole site endpoint is the victim.
	// Remote-to-remote and external paths remain required.
	SiteIsolated []string
	Name         string
	// Victim is the node that goes away.
	Victim string
	Mode   Mode
}

// Result is what a row observed, kept as values so a report is
// generated rather than parsed back out of a log.
type Result struct {
	Row       Row
	Baseline  *check.Matrix
	Survivors *check.Matrix
	Returned  *check.Matrix
	// Failed says which claim failed, empty when the row passed.
	Failed string
	Err    error
}

// OK reports whether the row passed.
func (r Result) OK() bool { return r.Failed == "" && r.Err == nil }

func (r Result) String() string {
	verdict := "PASS"
	detail := ""
	if !r.OK() {
		verdict = "FAIL"
		detail = "  " + r.Failed
		if r.Err != nil {
			detail += ": " + r.Err.Error()
		}
	}
	return fmt.Sprintf("### %s %s (victim=%s, mode=%s)%s",
		verdict, r.Row.Name, r.Row.Victim, r.Row.Mode, detail)
}

// Deps is what a row needs to run.
type Deps struct {
	// Observe records the actual survivor routes before restoring the victim.
	Observe func(context.Context, string, []string) error
	Rig     rig.Nodes
	Prober  check.Prober
	// Targets is every probe pod, including the victim's.
	Targets []check.Target
	Options check.Options
	// Converge is how long a claim has to become true. Bounded,
	// because the claim is not that these paths work eventually: it is
	// that they work within the time an operator would wait.
	Converge time.Duration
	// Down is how long to wait for the cluster to notice a node has
	// gone before measuring the survivors.
	Down time.Duration
	// Restart runs between the victim leaving and returning, so a
	// caller can re-plumb whatever the platform would have given back.
	Restart func(ctx context.Context, victim string) error
	// Refresh re-reads where the probe pods are, for the nodes given.
	//
	// The nodes given, and not all of them: while the victim is down
	// its probe cannot be ready, and waiting for it would fail the
	// row for the thing the row is doing on purpose.
	//
	// A pod dies with its node and comes back somewhere else, with a
	// different address. Measuring the address it had before the
	// outage reports the path as broken when what is broken is the
	// harness's memory of it — and it does so selectively, which is
	// worse: the service address still resolves to whatever is
	// serving now, so a stale pod address fails while the service
	// address beside it passes, and the pattern reads exactly like a
	// routing fault.
	Refresh func(ctx context.Context, nodes []string) ([]check.Target, error)
}

// Run executes one row.
func Run(ctx context.Context, row Row, d Deps) (res Result) {
	res = Result{Row: row}

	if row.Mode != Cut && row.Mode != Reboot {
		res.Failed = "invalid outage mode"
		return res
	}
	if !slices.Contains(names(d.Targets), row.Victim) {
		res.Failed = "victim is not in the measured matrix"
		return res
	}
	// Everything green before anything breaks.
	res.Baseline = check.Converge(ctx, d.Prober, d.Targets, d.Options, d.Converge, 10*time.Second)
	if !res.Baseline.OK() {
		res.Failed = "the baseline was already broken, so a failure after this would name the wrong culprit"
		return res
	}

	victim := d.Rig.Node(row.Victim)

	restored := false
	defer func() {
		if restored {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := bringBack(cleanup, victim, row.Mode, d.Restart); err != nil {
			res.Err = fmt.Errorf("%v; restoring victim after failure: %w", res.Err, err)
		}
	}()

	if err := takeDown(ctx, victim, row.Mode); err != nil {
		res.Failed, res.Err = "taking the victim down", err
		return res
	}

	// Give the cluster time to notice. Measuring the instant the link
	// drops measures the moment of the break rather than what the
	// survivors settle to.
	select {
	case <-ctx.Done():
		res.Failed, res.Err = "waiting for the victim to be noticed", ctx.Err()
		return res
	case <-time.After(d.Down):
	}

	// The survivors, among themselves. The victim's own pods are gone
	// with it, which is allowed; everyone else's must not be.
	// The survivors only: the victim is down, so its probe is not
	// ready and never will be until it returns.
	want := names(without(d.Targets, row.Victim))
	want = slices.DeleteFunc(want, func(n string) bool { return slices.Contains(row.SiteIsolated, n) })
	targets, err := d.refresh(ctx, want)
	if err != nil {
		res.Failed, res.Err = "re-reading where the surviving probes are", err
		return res
	}
	// Isolated remotes keep their running probe processes, observable over
	// serial even when Kubernetes can no longer report their readiness.
	if len(row.SiteIsolated) > 0 {
		targets = slices.DeleteFunc(slices.Clone(targets), func(t check.Target) bool { return slices.Contains(row.SiteIsolated, t.Node) })
		for _, target := range d.Targets {
			if slices.Contains(row.SiteIsolated, target.Node) {
				targets = append(targets, target)
			}
		}
	}
	survivorOptions := d.Options
	survivorOptions.NotRequired = func(from, to string, kind check.Kind) string {
		fromRemote, toRemote := slices.Contains(row.SiteIsolated, from), slices.Contains(row.SiteIsolated, to)
		if (to != "" && fromRemote != toRemote) || (kind == check.DNS && fromRemote) {
			return "sole site endpoint is down; site isolation is allowed"
		}
		return ""
	}
	survivors := without(targets, row.Victim)
	res.Survivors = check.Converge(ctx, d.Prober, survivors, survivorOptions, d.Converge, 10*time.Second)
	if d.Observe != nil {
		if err := d.Observe(ctx, "down", names(survivors)); err != nil {
			res.Failed, res.Err = "capturing outage observations", err
			return res
		}
	}
	if !res.Survivors.OK() {
		res.Failed = "the survivors did not converge among themselves: losing one node cost more than that node"
		return res
	}

	if err := bringBack(ctx, victim, row.Mode, d.Restart); err != nil {
		res.Failed, res.Err = "bringing the victim back", err
		return res
	}

	restored = true

	// And the whole cluster again, with nothing reinstalled. Every
	// node this time, the returned victim included.
	targets, err = d.refresh(ctx, names(d.Targets))
	if err != nil {
		res.Failed, res.Err = "re-reading where the probes are once the victim is back", err
		return res
	}
	res.Returned = check.Converge(ctx, d.Prober, targets, d.Options, d.Converge, 10*time.Second)
	if !res.Returned.OK() {
		res.Failed = "the cluster did not return to green after the victim came back"
		if res.Returned.Cancelled {
			res.Failed = "the run was cancelled before the cluster had its window to return, so this row settles nothing"
		}
		return res
	}
	return res
}

// refresh re-reads the probes, falling back to what the row started
// with when a caller offered no way to.
func (d Deps) refresh(ctx context.Context, nodes []string) ([]check.Target, error) {
	if d.Refresh == nil {
		return d.Targets, nil
	}
	return d.Refresh(ctx, nodes)
}

// names lists the nodes a set of targets covers.
func names(targets []check.Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Node)
	}
	return out
}

func takeDown(ctx context.Context, victim rig.Node, mode Mode) error {
	if mode == Reboot {
		return victim.Kill(ctx)
	}
	return victim.Cut(ctx)
}

// bringBack returns the victim, and then gives it the platform's
// share.
//
// Both, for either mode. A machine that was killed needs its NIC
// back; a machine whose cable was pulled has one, but a link that
// went down took its routes with it and nothing in the kernel puts
// them back. Restoring an address and not a gateway leaves a node
// reachable on its own segment and nowhere else, which reads exactly
// like the cluster failing to readmit it.
func bringBack(ctx context.Context, victim rig.Node, mode Mode, restart func(context.Context, string) error) error {
	if mode == Reboot {
		if err := victim.Boot(ctx); err != nil {
			return err
		}
	} else if err := victim.Restore(ctx); err != nil {
		return err
	}
	if restart != nil {
		return restart(ctx, victim.Name())
	}
	return nil
}

// without drops one node from the targets.
func without(targets []check.Target, node string) []check.Target {
	var out []check.Target
	for _, t := range targets {
		if t.Node != node {
			out = append(out, t)
		}
	}
	return out
}

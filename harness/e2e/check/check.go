// Package check is the reachability matrix: every ordered pair of
// nodes, by pod address and by service address, a transfer larger
// than any packet the path can carry, cluster DNS, and the path off
// the cluster.
//
// What it measures is the claim the product makes: that a node joined
// over a tunnel is an ordinary member of the pod network in both
// directions, and that tunnel placement is invisible from the pod
// network. A node with no tunnel reaches a remote by transiting one
// that has one; a row that fails is that claim being wrong.
//
// Two decisions here are load-bearing and both were learned the hard
// way.
//
// Probes run through the node's own container runtime, never through
// kubectl exec. kubectl exec reaches a pod by way of its node's
// kubelet, which the API server connects to directly, and a node
// joined over a tunnel has no return path for that: the control plane
// carries no tunnel, which is what keeps a tunnel from ever costing a
// control plane its default route. Using kubectl exec would report
// the tunnel as broken when it is the exec path that is absent.
//
// The transfer check exists because every other check fits in a
// single small packet. A path whose largest packet cannot cross looks
// perfectly healthy to a ping-sized probe, which is exactly what an
// encapsulating network stacking its own header inside the tunnel's
// produces.
package check

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind is what one check measured.
type Kind string

const (
	// Pod is one pod reaching another by pod address.
	Pod Kind = "pod"
	// Service is the same reach by service address. It matters
	// separately because a ClusterIP is translated to a backend on the
	// sending node, so no service range is permitted anywhere in the
	// tunnel's accept list. If that reasoning is wrong, these are the
	// checks that fail.
	Service Kind = "service"
	// Transfer is a body far larger than any packet on the path.
	Transfer Kind = "transfer"
	// DNS is cluster DNS from a node.
	DNS Kind = "dns"
	// External is the path off the cluster from a node.
	External Kind = "external"
)

// Result is one check.
type Result struct {
	From string
	To   string // empty for checks that are not about a pair
	Kind Kind
	OK   bool
	// Detail says what was observed when that is more than pass or
	// fail: how many bytes came back, what address was resolved.
	Detail string
	Err    error
}

// String renders the result the way a row's log reads.
func (r Result) String() string {
	verdict := "PASS"
	if !r.OK {
		verdict = "FAIL"
	}
	subject := r.From
	if r.To != "" {
		subject = r.From + " to " + r.To
	}
	line := fmt.Sprintf("  %s  %s %s", verdict, subject, r.Kind)
	if r.Detail != "" {
		line += " (" + r.Detail + ")"
	}
	if r.Err != nil {
		line += ": " + r.Err.Error()
	}
	return line
}

// Matrix is every check from one run.
//
// It is a value rather than a log file on purpose. The bash harness
// wrote verdicts as text and read them back by grepping for a marker,
// and a stale copy of one of those files was reported as a live
// result during the campaign this replaces.
type Matrix struct {
	mu      sync.Mutex
	results []Result
	// Elapsed is how long it took to become green, or how long was
	// spent failing to. Reported either way: a run that says only
	// "failed" leaves a reader unable to tell a broken path from one
	// that needed longer than it was given, and those call for
	// opposite responses.
	Elapsed time.Duration
	// Window is how long it was allowed.
	Window time.Duration
}

// Add records one result. Safe to call from many goroutines, which is
// the point: the pairs are independent and there is no reason to walk
// them one at a time.
func (m *Matrix) Add(r Result) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results = append(m.results, r)
}

// Results returns every check, ordered so that two runs of the same
// row produce comparable output.
func (m *Matrix) Results() []Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Result(nil), m.results...)
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].From != out[b].From {
			return out[a].From < out[b].From
		}
		if out[a].To != out[b].To {
			return out[a].To < out[b].To
		}
		return out[a].Kind < out[b].Kind
	})
	return out
}

// Total, Passed and Failed count the run.
func (m *Matrix) Total() int { return len(m.Results()) }

func (m *Matrix) Passed() int {
	n := 0
	for _, r := range m.Results() {
		if r.OK {
			n++
		}
	}
	return n
}

func (m *Matrix) Failed() int { return m.Total() - m.Passed() }

// Failures returns only what failed, which is what a report should
// lead with.
func (m *Matrix) Failures() []Result {
	var out []Result
	for _, r := range m.Results() {
		if !r.OK {
			out = append(out, r)
		}
	}
	return out
}

// OK reports whether the run passed.
//
// Zero checks is not a pass. A check that never ran proves nothing,
// and a harness that reports "no checks ran" as success is how a
// broken configuration stays green: this happened, on a healthy
// network, when a namespace from a previous run would not delete.
func (m *Matrix) OK() bool { return m.Total() > 0 && m.Failed() == 0 }

// Summary is the one line a row's log carries.
func (m *Matrix) Summary() string {
	line := fmt.Sprintf("checks: %d  passed: %d  failed: %d", m.Total(), m.Passed(), m.Failed())
	switch {
	case m.Elapsed == 0:
		return line
	case m.OK():
		return fmt.Sprintf("%s  converged after %s", line, m.Elapsed.Round(time.Second))
	default:
		return fmt.Sprintf("%s  gave up after %s of %s", line,
			m.Elapsed.Round(time.Second), m.Window.Round(time.Second))
	}
}

// Report renders the failures and the summary.
func (m *Matrix) Report() string {
	var b strings.Builder
	for _, r := range m.Failures() {
		b.WriteString(r.String())
		b.WriteString("\n")
	}
	b.WriteString("    " + m.Summary())
	return b.String()
}

// Pair is one ordered pair of nodes.
type Pair struct{ From, To string }

// Pairs enumerates every ordered pair of distinct nodes.
//
// Ordered, and every one of them: direction matters, because a tunnel
// is dialled from one side and the return path is not the same
// mechanism. Sampling a subset is how a transit pair went unmeasured
// long enough for a placement change to strand it unnoticed.
func Pairs(nodes []string) []Pair {
	var out []Pair
	for _, from := range nodes {
		for _, to := range nodes {
			if from != to {
				out = append(out, Pair{From: from, To: to})
			}
		}
	}
	return out
}

// Prober runs one probe from a node and reports what came back.
type Prober interface {
	// HTTPGet fetches url from within the probe pod on node and
	// returns the body.
	HTTPGet(ctx context.Context, node, url string) ([]byte, error)
	// Resolve looks up name from within the probe pod on node.
	Resolve(ctx context.Context, node, name string) error
}

// Target is where a node's probe pod can be reached.
type Target struct {
	Node      string
	PodIP     string
	ServiceIP string
}

// TransferBytes is the body the transfer check asks for. A megabyte
// is far past any MTU on this path, so a short read or a stall is a
// packet-size failure and nothing else.
const TransferBytes = 1024 * 1024

// Concurrency bounds how many probes are in flight. The pairs are
// independent, so the only reason to bound this at all is that each
// probe costs an exec on a node and a node has finite patience.
const Concurrency = 16

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
// Workload probes use the node's container runtime through out-of-band
// management. This keeps pod-network failures distinguishable from failures
// of the API server's kubelet connection. Kubernetes exec and logs require
// a separate management-path check; a passing workload matrix cannot prove them.
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

// ExternalURL stays on a literal IP. The root at http://1.1.1.1 redirects to
// one.one.one.one, which incorrectly makes an external-path check depend on
// cluster DNS while the sole site endpoint is down. Observed on k0s/kube-router.
const ExternalURL = "https://1.1.1.1/cdn-cgi/trace"

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

	// Pod6, Service6 and Transfer6 are Pod, Service and Transfer over
	// IPv6, for a dual-stack pair. DNS6 is cluster DNS answering with
	// the target Service's IPv6 address.
	Pod6      Kind = "pod6"
	Service6  Kind = "service6"
	Transfer6 Kind = "transfer6"
	DNS6      Kind = "dns6"
	// UDP and UDP6 are exact UDP echoes of one datagram size, by pod
	// address, in each family. See UDPProbes.
	UDP  Kind = "udp"
	UDP6 Kind = "udp6"
)

// Family is an IP address family.
type Family string

const (
	IPv4 Family = "IPv4"
	IPv6 Family = "IPv6"
)

// Result is one check.
type Result struct {
	NotRequired string
	From        string
	To          string // empty for checks that are not about a pair
	Kind        Kind
	// Size is the whole IP datagram a UDP check sent, zero otherwise.
	Size int
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
	if r.NotRequired != "" {
		verdict = "NOT REQUIRED"
		r.Detail = r.NotRequired
	}
	subject := r.From
	if r.To != "" {
		subject = r.From + " to " + r.To
	}
	line := fmt.Sprintf("  %s  %s %s", verdict, subject, r.Kind)
	if r.Size != 0 {
		line += fmt.Sprintf(" %dB", r.Size)
	}
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
	// Cancelled says the run was stopped from outside rather than
	// having used up its window.
	//
	// Reported separately because the two mean opposite things: a run
	// that used its window measured something and found it wanting, a
	// run that was cancelled measured nothing conclusive at all. Not
	// distinguishing them is what makes a reader reach for a shell to
	// find out, and a result that needs a human with a shell is not a
	// result.
	Cancelled bool
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
		if out[a].Kind != out[b].Kind {
			return out[a].Kind < out[b].Kind
		}
		return out[a].Size < out[b].Size
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

func (m *Matrix) NotRequired() int {
	n := 0
	for _, r := range m.Results() {
		if r.NotRequired != "" {
			n++
		}
	}
	return n
}
func (m *Matrix) Failed() int { return m.Total() - m.Passed() - m.NotRequired() }

// Failures returns only what failed, which is what a report should
// lead with.
func (m *Matrix) Failures() []Result {
	var out []Result
	for _, r := range m.Results() {
		if !r.OK && r.NotRequired == "" {
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
func (m *Matrix) OK() bool { return !m.Cancelled && m.Passed() > 0 && m.Failed() == 0 }

// Summary is the one line a row's log carries.
func (m *Matrix) Summary() string {
	line := fmt.Sprintf("checks: %d  passed: %d  failed: %d", m.Total(), m.Passed(), m.Failed())
	if n := m.NotRequired(); n > 0 {
		line += fmt.Sprintf("  not required: %d", n)
	}
	switch {
	case m.Elapsed == 0:
		return line
	case m.OK():
		return fmt.Sprintf("%s  converged after %s", line, m.Elapsed.Round(time.Second))
	case m.Cancelled:
		return fmt.Sprintf("%s  CUT SHORT after %s of %s: the run was cancelled, so this settles nothing",
			line, m.Elapsed.Round(time.Second), m.Window.Round(time.Second))
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
	for _, r := range m.Results() {
		if r.NotRequired != "" {
			b.WriteString(r.String() + "\n")
		}
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

// TransferProber measures the received body inside the probe pod. This avoids
// carrying a large response through command transports with output limits.
// Implementations must send TransferBytes, fetch the full echoed body and
// propagate failures in either direction.
type TransferProber interface {
	HTTPSize(ctx context.Context, node, url string) (int64, error)
}

// LookupProber resolves a name in one family from within the probe pod on
// node and returns the answers.
type LookupProber interface {
	Lookup(ctx context.Context, node, name string, family Family) ([]string, error)
}

// UDPProber sends tries datagrams of payload bytes, each from a fresh socket,
// from the probe pod on node to destination's echo listener and reports every
// attempt. dontFragment sets the don't-fragment bit and ignores any learned
// path MTU, so a datagram the path cannot carry is lost every time rather
// than only until the sender learns to fragment it.
type UDPProber interface {
	UDPEcho(ctx context.Context, node, destination string, payload, tries int, dontFragment bool) (UDPReport, error)
}

// UDPReport is what came back from one UDPEcho.
type UDPReport struct {
	OK       bool
	Attempts int
	Echoed   int
	Errors   []string
}

// Target is where a node's probe pod can be reached. The IPv6 fields are
// set for a dual-stack pod and Service; ServiceName is the Service's
// fully qualified DNS name.
type Target struct {
	Node        string
	PodIP       string
	ServiceIP   string
	PodIP6      string
	ServiceIP6  string
	ServiceName string
	// MTU is the probe pod's own interface MTU as read inside it, zero
	// when it could not be read.
	MTU int
}

// UDPOptions selects the UDP size probes.
type UDPOptions struct {
	// PodMTU is the MTU of the pods' own interfaces, the largest datagram
	// a pod sends without fragmenting it. Zero probes each pair around
	// the smaller of its two pods' measured MTUs (Target.MTU).
	PodMTU int
	// Tries is how many datagrams of each size are sent.
	Tries int
}

// UDPProbe is one datagram size, as the whole IP datagram.
type UDPProbe struct {
	Datagram     int
	DontFragment bool
}

// udpEchoPrefix is the command the echo listener strips before replying,
// and udpMaxPayload is what fits its 2048-byte receive buffer with it.
const (
	udpEchoPrefix  = len("echo ")
	udpMaxPayload  = 2048 - udpEchoPrefix
	udpMinPayload  = 24
	udpLargeProbe  = 1800
	ipv6MinimumMTU = 1280
)

// Payload is the echo body that makes a datagram of p.Datagram bytes in
// family: the datagram less the IP header (20 or 40 bytes), the UDP header
// and the echo command.
func (p UDPProbe) Payload(family Family) int {
	header := 20
	if family == IPv6 {
		header = 40
	}
	return p.Datagram - header - 8 - udpEchoPrefix
}

// UDPProbes are the sizes measured around the pod MTU. With the
// don't-fragment bit: the IPv6 minimum MTU, and the largest datagram a pod
// sends unfragmented and one byte less, which every path must carry whole.
// Without it: one byte over the pod MTU and a large datagram, which the
// sender fragments and every path must carry in pieces.
func UDPProbes(o UDPOptions) []UDPProbe {
	var out []UDPProbe
	seen := map[int]bool{}
	add := func(size int, df bool) {
		if seen[size] || size <= 0 {
			return
		}
		probe := UDPProbe{Datagram: size, DontFragment: df}
		if probe.Payload(IPv4) < udpMinPayload || probe.Payload(IPv6) > udpMaxPayload {
			return
		}
		seen[size] = true
		out = append(out, probe)
	}
	for _, size := range []int{ipv6MinimumMTU, o.PodMTU - 1, o.PodMTU} {
		if size <= o.PodMTU {
			add(size, true)
		}
	}
	add(o.PodMTU+1, false)
	if udpLargeProbe > o.PodMTU+1 {
		add(udpLargeProbe, false)
	}
	return out
}

// TransferBytes is the body the transfer check asks for. A megabyte
// is far past any MTU on this path, so a short read or a stall is a
// packet-size failure and nothing else.
const TransferBytes = 1024 * 1024

// Concurrency bounds how many probes are in flight. The pairs are
// independent, so the only reason to bound this at all is that each
// probe costs an exec on a node and a node has finite patience.
const Concurrency = 16

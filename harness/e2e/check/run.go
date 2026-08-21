package check

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Run measures every ordered pair and every per-node check.
//
// Concurrently, bounded. The bash harness walked the pairs in nested
// loops, one exec at a time: on a seven-node row that is 161 checks
// in sequence, and the wall clock it cost is what made a full
// campaign an overnight job. Nothing about a pair depends on any
// other pair.
func Run(ctx context.Context, p Prober, targets []Target, opts Options) *Matrix {
	m := &Matrix{}
	byNode := map[string]Target{}
	var names []string
	for _, t := range targets {
		byNode[t.Node] = t
		names = append(names, t.Node)
	}

	sem := make(chan struct{}, opts.concurrency())
	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			f()
		}()
	}

	for _, pair := range Pairs(names) {
		dst := byNode[pair.To]
		run(func() { m.Add(reach(ctx, p, pair, Pod, url(dst.PodIP, opts.Port, ""))) })
		run(func() { m.Add(reach(ctx, p, pair, Service, url(dst.ServiceIP, opts.Port, ""))) })
		if !opts.SkipTransfer {
			run(func() { m.Add(transfer(ctx, p, pair, url(dst.PodIP, opts.Port, "big"))) })
		}
	}

	for _, name := range names {
		run(func() { m.Add(resolve(ctx, p, name)) })
		if opts.ExternalURL != "" {
			run(func() { m.Add(external(ctx, p, name, opts.ExternalURL)) })
		}
	}

	wg.Wait()
	return m
}

// Options tunes a run.
type Options struct {
	// Port the probe pods serve on.
	Port int
	// ExternalURL is the path off the cluster. Empty skips it, for a
	// lab with no route out.
	ExternalURL string
	// SkipTransfer drops the large-body check. Only for a run that is
	// measuring something else and needs to be quick; a row must not
	// skip it, because it is the only check that can see a path whose
	// largest packet does not cross.
	SkipTransfer bool
	// Concurrency overrides the default bound.
	Concurrency int
}

func (o Options) concurrency() int {
	if o.Concurrency > 0 {
		return o.Concurrency
	}
	return Concurrency
}

func url(host string, port int, path string) string {
	return fmt.Sprintf("http://%s:%d/%s", host, port, path)
}

// reach is the small-packet check: one request, one known body.
func reach(ctx context.Context, p Prober, pair Pair, kind Kind, target string) Result {
	body, err := p.HTTPGet(ctx, pair.From, target)
	r := Result{From: pair.From, To: pair.To, Kind: kind}
	switch {
	case err != nil:
		r.Err = err
	case strings.TrimSpace(string(body)) != "ok":
		r.Detail = fmt.Sprintf("body %q", truncate(string(body)))
	default:
		r.OK = true
	}
	return r
}

// transfer is the large-body check. It asserts the exact length,
// because a body that comes back short is the failure this exists to
// catch and a body that merely arrives is not evidence of anything.
func transfer(ctx context.Context, p Prober, pair Pair, target string) Result {
	body, err := p.HTTPGet(ctx, pair.From, target)
	r := Result{From: pair.From, To: pair.To, Kind: Transfer}
	switch {
	case err != nil:
		r.Err = err
		r.Detail = fmt.Sprintf("want %d bytes", TransferBytes)
	case len(body) != TransferBytes:
		r.Detail = fmt.Sprintf("%d of %d bytes", len(body), TransferBytes)
	default:
		r.OK = true
		r.Detail = fmt.Sprintf("%d bytes", len(body))
	}
	return r
}

func resolve(ctx context.Context, p Prober, node string) Result {
	err := p.Resolve(ctx, node, "kubernetes.default.svc.cluster.local")
	return Result{From: node, Kind: DNS, OK: err == nil, Err: err}
}

func external(ctx context.Context, p Prober, node, target string) Result {
	_, err := p.HTTPGet(ctx, node, target)
	return Result{From: node, Kind: External, OK: err == nil, Err: err}
}

func truncate(s string) string {
	const max = 40
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// Converge runs the matrix until it is green or the window runs out,
// and returns the last one it ran.
//
// A single pass measures a moment, not a state. Routes converge
// asynchronously — a remote's pod block is published after it joins,
// the transit for a node with no tunnel follows an election, and a
// network's own agent programs its routes on its own schedule — so a
// matrix taken the instant a node appears reports a lab that is still
// assembling itself as a lab that is broken.
//
// The window is bounded and its expiry is a failure, because the
// claim is not that these paths work eventually: it is that they work
// within the time an operator would wait.
func Converge(ctx context.Context, p Prober, targets []Target, opts Options, within, every time.Duration) *Matrix {
	deadline := time.Now().Add(within)
	for {
		m := Run(ctx, p, targets, opts)
		if m.OK() || time.Now().After(deadline) || ctx.Err() != nil {
			return m
		}
		select {
		case <-ctx.Done():
			return m
		case <-time.After(every):
		}
	}
}

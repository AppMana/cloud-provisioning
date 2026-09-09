package rig

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type completionNode struct {
	Node
	raw                  string
	statusErr, markerErr error
	markerReads          int
}

func (n *completionNode) Name() string { return "worker" }
func (n *completionNode) Exec(_ context.Context, args ...string) ([]byte, error) {
	switch args[0] {
	case "cloud-init":
		return []byte(n.raw), n.statusErr
	case "test":
		if strings.Join(args, " ") != "test -f /run/cluster-api/bootstrap-success.complete" {
			panic("unexpected marker command")
		}
		n.markerReads++
		return nil, n.markerErr
	default:
		panic("unexpected command")
	}
}
func (n *completionNode) BootstrapComplete(ctx context.Context) (bool, error) {
	return CloudInitComplete(ctx, n)
}
func TestCompletedBootstrapRequiresNativeSuccessAndMarker(t *testing.T) {
	for _, tc := range []struct {
		name, raw            string
		statusErr, markerErr error
		complete, terminal   bool
		markerReads          int
	}{
		{name: "observed worker race", raw: `{"status":"running"}`},
		{name: "disabled retained image", raw: `{"status":"disabled"}`},
		{name: "complete without sentinel", raw: `{"status":"done"}`, markerErr: &ExitError{Code: 1}, markerReads: 1},
		{name: "complete with sentinel", raw: `{"status":"done"}`, complete: true, markerReads: 1},
		{name: "terminal native failure", raw: `{"status":"error","errors":["private userdata"]}`, statusErr: &ExitError{Code: 1}, terminal: true},
		{name: "transport failure", raw: `{"status":"error"}`, statusErr: errors.New("transport private data")},
		{name: "partial status", raw: `{"status":"done"`},
		{name: "recoverable native errors", raw: `{"status":"done"}`, statusErr: &ExitError{Code: 2}},
		{name: "marker transport failure", raw: `{"status":"done"}`, markerErr: errors.New("transport"), markerReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &completionNode{raw: tc.raw, statusErr: tc.statusErr, markerErr: tc.markerErr}
			done, err := n.BootstrapComplete(context.Background())
			if done != tc.complete || (err != nil) != tc.terminal || n.markerReads != tc.markerReads {
				t.Fatalf("done=%v err=%v marker reads=%d", done, err, n.markerReads)
			}
			if err != nil && strings.Contains(err.Error(), "private userdata") {
				t.Fatal("private payload leaked")
			}
		})
	}
}
func TestWaitBootstrapCompletion(t *testing.T) {
	if err := WaitBootstrap(context.Background(), &completionNode{raw: `{"status":"done"}`}, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := WaitBootstrap(context.Background(), &healthNode{}, time.Second); err == nil {
		t.Fatal("unsupported observer passed")
	}
	if err := WaitBootstrap(context.Background(), &completionNode{raw: `{"status":"error"}`}, time.Minute); err == nil {
		t.Fatal("terminal bootstrap passed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitBootstrap(ctx, &completionNode{raw: `{"status":"running"}`}, time.Second); err == nil {
		t.Fatal("running bootstrap passed")
	}
}

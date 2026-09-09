package aws

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type capaCompletionNode struct {
	rig.Node
	raw            []byte
	err, markerErr error
}

func (n *capaCompletionNode) Name() string { return "capa-worker" }
func (n *capaCompletionNode) Exec(_ context.Context, args ...string) ([]byte, error) {
	if args[0] == "cloud-init" {
		return n.raw, n.err
	}
	if strings.Join(args, " ") != "test -f /run/cluster-api/bootstrap-success.complete" {
		panic("unexpected command")
	}
	return nil, n.markerErr
}

func TestCAPACompletionFromObservedCloudInit(t *testing.T) {
	raw, err := os.ReadFile("testdata/capa-cloud-init-26.1-include-warning.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, raw      string
		err, markerErr error
		want, fatal    bool
	}{
		{name: "observed secure include", raw: string(raw), err: &rig.ExitError{Code: 2}, want: true},
		{name: "generic clean completion", raw: `{"status":"done"}`, want: true},
		{name: "missing marker", raw: string(raw), err: &rig.ExitError{Code: 2}, markerErr: &rig.ExitError{Code: 1}},
		{name: "marker transport failure", raw: string(raw), err: &rig.ExitError{Code: 2}, markerErr: errors.New("transport")},
		{name: "transport", raw: string(raw), err: errors.New("transport")},
		{name: "wrong exit", raw: string(raw), err: &rig.ExitError{Code: 1}},
		{name: "unknown warning", raw: strings.ReplaceAll(string(raw), capaIncludeWarning, "unrecognized warning"), err: &rig.ExitError{Code: 2}},
		{name: "terminal", raw: `{"status":"error"}`, err: &rig.ExitError{Code: 1}, fatal: true},
		{name: "malformed", raw: `{`, err: &rig.ExitError{Code: 2}},
		{name: "running", raw: strings.Replace(string(raw), `"status": "done"`, `"status": "running"`, 1), err: &rig.ExitError{Code: 2}},
		{name: "hidden module error", raw: strings.Replace(string(raw), `"errors": []`, `"errors": ["private userdata"]`, 2), err: &rig.ExitError{Code: 2}},
		{name: "missing stage", raw: strings.Replace(string(raw), `"modules-final"`, `"unknown-stage"`, 1), err: &rig.ExitError{Code: 2}},
		{name: "wrong severity", raw: strings.ReplaceAll(string(raw), `"WARNING"`, `"ERROR"`), err: &rig.ExitError{Code: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &capaCompletionNode{raw: []byte(tc.raw), err: tc.err, markerErr: tc.markerErr}
			got, err := (CAPALinuxCommands{}).BootstrapComplete(context.Background(), n)
			if got != tc.want || (err != nil) != tc.fatal {
				t.Fatalf("complete=%v error=%v", got, err)
			}
			if err != nil && strings.Contains(err.Error(), "private userdata") {
				t.Fatal("private error leaked")
			}
		})
	}
	n := &capaCompletionNode{raw: raw, err: &rig.ExitError{Code: 2}}
	if got, err := (LinuxCommands{}).BootstrapComplete(context.Background(), n); got || err != nil {
		t.Fatal("CAPA exception leaked into generic Linux adapter")
	}
}

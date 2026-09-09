package rig

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

type healthNode struct {
	Node
	raw  []byte
	err  error
	args []string
}

func (n *healthNode) Name() string { return "remote1" }
func (n *healthNode) Exec(_ context.Context, args ...string) ([]byte, error) {
	n.args = args
	return n.raw, n.err
}

func TestCloudInitObservedTerminalFailure(t *testing.T) {
	raw, err := os.ReadFile("testdata/cloud-init-terminal-error.json")
	if err != nil {
		t.Fatal(err)
	}
	n := &healthNode{raw: raw, err: &ExitError{Code: 1, Stderr: []byte("private userdata")}}
	err = CloudInitFailure(context.Background(), n)
	if err == nil || !strings.Contains(err.Error(), "terminal bootstrap error") {
		t.Fatalf("failure not classified: %v", err)
	}
	if strings.Contains(err.Error(), "redacted") || strings.Contains(err.Error(), "private userdata") {
		t.Fatal("bootstrap payload leaked")
	}
	if !reflect.DeepEqual(n.args, []string{"cloud-init", "status", "--format", "json"}) {
		t.Fatalf("command: %v", n.args)
	}
}
func TestCloudInitInconclusiveAndNonterminalObservations(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		err       error
	}{
		{"transport error", `{"status":"error"}`, errors.New("connection failed")},
		{"partial response", `{"status":"error"`, &ExitError{Code: 1}},
		{"unsupported", `command not found`, &ExitError{Code: 127}},
		{"running", `{"status":"running","errors":["private payload"]}`, nil},
		{"done", `{"status":"done"}`, nil},
		{"disabled on retained site VM", `{"status":"disabled","extended_status":"disabled"}`, nil},
		{"unknown", `{}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CloudInitFailure(context.Background(), &healthNode{raw: []byte(tc.raw), err: tc.err}); err != nil {
				t.Fatalf("inconclusive status marked terminal: %v", err)
			}
		})
	}
}

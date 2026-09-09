package aws

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

type observedSSM struct {
	command  string
	output   string
	stderr   string
	pending  bool
	timeout  string
	document string
}

func (a *observedSSM) Call(_ context.Context, service, op string, input map[string]any) (json.RawMessage, error) {
	if service == "ssm" && op == "send-command" {
		a.command = input["Parameters"].(map[string][]string)["commands"][0]
		a.timeout = input["Parameters"].(map[string][]string)["executionTimeout"][0]
		a.document = input["DocumentName"].(string)
		return json.RawMessage(`{"Command":{"CommandId":"test-command"}}`), nil
	}
	if !a.pending {
		a.pending = true
		return nil, ErrInvocationPending
	}
	return json.Marshal(map[string]any{"Status": "Success", "ResponseCode": 0, "StandardOutputContent": a.output, "StandardErrorContent": a.stderr})
}

func TestSSMExecutionDeadlineAppliesToBothGuestTypes(t *testing.T) {
	for _, commands := range []GuestCommands{LinuxCommands{}, WindowsCommands{}} {
		t.Run(commands.Document(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
			defer cancel()
			api := &observedSSM{pending: true}
			node := &Node{API: api, Commands: commands, NodeName: "image-builder"}
			if _, err := node.Exec(ctx, "image-preparation"); err != nil {
				t.Fatal(err)
			}
			seconds, err := strconv.Atoi(api.timeout)
			if err != nil || seconds < 2990 || seconds > 3000 || api.document != commands.Document() {
				t.Fatalf("caller deadline was not sent to the guest document: %q %q", api.timeout, api.document)
			}
		})
	}
}

func TestSSMExecutionTimeoutDefaultsAndLimit(t *testing.T) {
	if value, err := ssmExecutionTimeout(context.Background()); err != nil || value != "600" {
		t.Fatalf("default timeout: %q %v", value, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 72*time.Hour)
	defer cancel()
	if value, err := ssmExecutionTimeout(ctx); err != nil || value != "172800" {
		t.Fatalf("AWS execution limit: %q %v", value, err)
	}
}

func TestCancelledSSMCommandIsNotSubmitted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	node := &Node{NodeName: "worker"} // A submission through its nil API would panic.
	if _, err := node.Exec(ctx, "must-not-run"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled command: %v", err)
	}
}

func TestObservationTimeoutRetainsRemoteCommandHandle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	node := &Node{API: &observedSSM{}, NodeName: "image-builder"}
	_, err := node.Exec(ctx, "long-image-preparation")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "test-command") {
		t.Fatalf("cannot inspect the still-running remote command after observation ends: %v", err)
	}
}

func TestSSMExecPreservesArgvAndWaitsForInvocation(t *testing.T) {
	a := &observedSSM{output: "literal result"}
	n := &Node{API: a, NodeName: "worker", InstanceID: "i-test", NIC: "ens5"}
	arg := "a 'quote' $(printf injected); with spaces"
	out, err := n.Exec(context.Background(), "printf", "%s", arg)
	if err != nil || string(out) != "literal result" {
		t.Fatalf("SSM result: %s %v", out, err)
	}
	actual, err := exec.Command("sh", "-c", a.command).Output()
	if err != nil || string(actual) != arg {
		t.Fatalf("argv changed across SSM shell: %q %v", actual, err)
	}
}

func TestUnsupportedEC2OperationsDoNotCallGuest(t *testing.T) {
	n := &Node{NodeName: "worker"} // no API: unsupported operations must be local failures.
	for _, err := range []error{n.Cut(context.Background()), n.Restore(context.Background()), n.Userdata(context.Background(), []byte("secret"))} {
		if err == nil {
			t.Fatal("unsupported operation accepted")
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatal("error leaked bootstrap bytes")
		}
	}
}

func TestSSMTruncatedOutputIsNotEvidence(t *testing.T) {
	for _, channel := range []string{"stdout", "stderr"} {
		t.Run(channel, func(t *testing.T) {
			a := &observedSSM{pending: true}
			if channel == "stdout" {
				a.output = strings.Repeat("x", 24000)
			} else {
				a.stderr = strings.Repeat("y", 8000)
			}
			n := &Node{API: a, NodeName: "worker"}
			out, err := n.Exec(context.Background(), "true")
			if err == nil || len(out) != 0 {
				t.Fatal("accepted SSM output at truncation limit")
			}
			if !strings.Contains(err.Error(), "test-command") || !strings.Contains(err.Error(), channel+"=") {
				t.Fatalf("missing original invocation handle or channel size: %v", err)
			}
			if strings.Contains(err.Error(), "xxxxxxxx") || strings.Contains(err.Error(), "yyyyyyyy") {
				t.Fatal("error leaked command output")
			}
		})
	}
}

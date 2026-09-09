package aws

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWindowsDoesNotUseLinuxCloudInitObserver(t *testing.T) {
	// A nil node would panic if the Linux probe were accidentally dispatched.
	if err := (WindowsCommands{}).BootstrapFailure(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

type bootstrapSSM struct{ command string }

func (a *bootstrapSSM) Call(_ context.Context, service, op string, input map[string]any) (json.RawMessage, error) {
	if op == "send-command" {
		a.command = input["Parameters"].(map[string][]string)["commands"][0]
		return json.RawMessage(`{"Command":{"CommandId":"bootstrap-status"}}`), nil
	}
	return json.RawMessage(`{"Status":"Failed","ResponseCode":1,"StandardOutputContent":"{\"status\":\"error\"}","StandardErrorContent":"private userdata"}`), nil
}
func TestLinuxBootstrapFailureUsesSSMAndPreservesErrorOutput(t *testing.T) {
	api := &bootstrapSSM{}
	n := &Node{API: api, NodeName: "worker"}
	err := n.BootstrapFailure(context.Background())
	if err == nil || strings.Contains(err.Error(), "private userdata") {
		t.Fatalf("terminal failure: %v", err)
	}
	if api.command != "exec 'cloud-init' 'status' '--format' 'json'" {
		t.Fatalf("wrong guest probe: %s", api.command)
	}
}

func TestWindowsCompletionDoesNotDispatchLinuxProbe(t *testing.T) {
	n := &Node{NodeName: "windows", Commands: WindowsCommands{}}
	done, err := n.BootstrapComplete(context.Background())
	if done || err != nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
}
func TestLinuxCompletionClassifiesNativeFailure(t *testing.T) {
	n := &Node{API: &bootstrapSSM{}, NodeName: "worker"}
	done, err := n.BootstrapComplete(context.Background())
	if done || err == nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
}

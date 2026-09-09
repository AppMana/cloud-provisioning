package aws

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

func observedReceipt(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/windows-bootstrap-receipts.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Case    string         `json:"case"`
		Receipt map[string]any `json:"receipt"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		body, _ := json.Marshal(row.Receipt)
		done, err := receiptResult(body, row.Receipt["instanceID"].(string))
		if done != (row.Case == "success") || (err != nil) != (row.Case == "script-failure") {
			t.Fatalf("native %s done=%v err=%v", row.Case, done, err)
		}
	}
	return rows[0].Receipt
}
func TestReceiptClassificationUsesNativeWindowsCases(t *testing.T) { observedReceipt(t) }
func TestReceiptCannotPassWithoutBoundCompleteObservation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"foreign image receipt", func(r map[string]any) { r["instanceID"] = "i-stale" }},
		{"missing schema", func(r map[string]any) { delete(r, "schemaVersion") }},
		{"unknown schema", func(r map[string]any) { r["schemaVersion"] = 2 }},
		{"missing payload observation", func(r map[string]any) { delete(r, "payloadPresent") }},
		{"credential script remains", func(r map[string]any) { r["payloadPresent"] = true }},
		{"unprotected receipt", func(r map[string]any) { r["protectedRoot"] = false }},
		{"bad timestamp", func(r map[string]any) { r["updatedAt"] = "invalid" }},
		{"bad bootstrap reference", func(r map[string]any) { r["bootstrapID"] = "invalid" }},
		{"running", func(r map[string]any) { r["state"] = "running" }},
		{"incomplete stage", func(r map[string]any) { r["stage"] = "executing" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := observedReceipt(t)
			id := r["instanceID"].(string)
			tc.mutate(r)
			raw, _ := json.Marshal(r)
			done, err := receiptResult(raw, id)
			if done || err != nil {
				t.Fatalf("done=%v err=%v", done, err)
			}
		})
	}
	for _, raw := range []string{"", `{"schemaVersion":1`, "agent ran successfully"} {
		if done, err := receiptResult([]byte(raw), "i-worker"); done || err != nil {
			t.Fatalf("partial/agent output passed: %v %v", done, err)
		}
	}
}

type receiptNode struct {
	rig.Node
	raw  []byte
	err  error
	args []string
	id   string
}

func (n *receiptNode) InstanceIdentity() string { return n.id }
func (n *receiptNode) Exec(_ context.Context, args ...string) ([]byte, error) {
	n.args = args
	return n.raw, n.err
}
func TestWindowsCommandsUseReceiptNotLinuxStatus(t *testing.T) {
	r := observedReceipt(t)
	raw, _ := json.Marshal(r)
	n := &receiptNode{raw: raw, id: r["instanceID"].(string)}
	done, err := (WindowsCommands{}).BootstrapComplete(context.Background(), n)
	if !done || err != nil {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if !strings.Contains(strings.Join(n.args, " "), "Bootstrap") || strings.Contains(strings.Join(n.args, " "), "cloud-init") {
		t.Fatalf("wrong observer command")
	}
	n.err = errors.New("private transport detail")
	if done, err := windowsBootstrapResult(context.Background(), n); done || err != nil {
		t.Fatalf("transport failure passed")
	}
	r["state"] = "failed"
	r["stage"] = "executing"
	r["privateError"] = "PRIVATE_SENTINEL"
	n.raw, _ = json.Marshal(r)
	n.err = nil
	if err := (WindowsCommands{}).BootstrapFailure(context.Background(), n); err == nil || strings.Contains(err.Error(), "PRIVATE_SENTINEL") {
		t.Fatalf("failure payload classification: %v", err)
	}
}

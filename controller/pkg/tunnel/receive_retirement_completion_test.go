package tunnel

import (
	"encoding/json"
	"testing"
)

// Schema validation only; no record here authenticates a consumer or proves
// native completion. The withdrawal owner validates evidence semantics.
func TestReceiveRetirementCompletionRequiresWithdrawal(t *testing.T) {
	for _, raw := range []string{"", `{}`, `null`, `[]`, `true`, `"evidence"`, `{} {}`} {
		t.Run(raw, func(t *testing.T) {
			v := ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: "test", MeshName: "mesh", MeshUID: "mesh-uid", MachineUIDs: []string{"machine"}, Publications: map[string]ReceiveJournalPublication{"machine": {JournalUID: "journal", Retirement: json.RawMessage(`{}`), RetirementCompletion: json.RawMessage(raw)}}}
			err := v.validate()
			if raw == "" || raw == `{}` {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid completion accepted")
			}
			if raw == `{}` {
				p := v.Publications["machine"]
				p.Retirement = nil
				v.Publications["machine"] = p
				if err := v.validate(); err == nil {
					t.Fatal("completion without persisted withdrawal accepted")
				}
			}
		})
	}
}

// Format validation grants no finalization or release authority.
func TestReceiveRetirementFinalizationRequiresCompletion(t *testing.T) {
	for _, raw := range []string{"", `{}`, `null`, `[]`, `true`, `{} {}`} {
		v := ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: "test", MeshName: "mesh", MeshUID: "mesh-uid", MachineUIDs: []string{"machine"}, Publications: map[string]ReceiveJournalPublication{"machine": {JournalUID: "journal", Retirement: json.RawMessage(`{}`), RetirementCompletion: json.RawMessage(`{}`), RetirementFinalization: json.RawMessage(raw)}}}
		err := v.validate()
		if raw == "" || raw == `{}` {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil {
			t.Fatal("invalid finalization accepted", raw)
		}
		if raw == `{}` {
			p := v.Publications["machine"]
			p.RetirementCompletion = nil
			v.Publications["machine"] = p
			if v.validate() == nil {
				t.Fatal("finalization without consumed completion accepted")
			}
		}
	}
}

package tunnel

import (
	"encoding/json"
	"testing"
)

// Schema checks do not authenticate release authority or qualify native reuse.
func TestReleasedReceiveJournalSchema(t *testing.T) {
	for _, fault := range []string{"valid", "active-reservation", "active-publication", "missing-journal", "prepared-plan", "missing-intent", "missing-completion", "missing-finalization", "null", "array", "trailing", "blank-owner"} {
		t.Run(fault, func(t *testing.T) {
			uid := "retired"
			p := ReceiveJournalPublication{JournalUID: "journal", Retirement: json.RawMessage(`{}`), RetirementCompletion: json.RawMessage(`{}`), RetirementFinalization: json.RawMessage(`{}`)}
			v := ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: "test", MeshName: "mesh", MeshUID: "mesh-uid", MachineUIDs: []string{}}
			switch fault {
			case "active-reservation":
				v.MachineUIDs = []string{uid}
			case "active-publication":
				v.MachineUIDs = []string{uid}
				v.Publications = map[string]ReceiveJournalPublication{uid: p}
			case "missing-journal":
				p.JournalUID = ""
			case "prepared-plan":
				p.Plan = json.RawMessage(`{}`)
			case "missing-intent":
				p.Retirement = nil
			case "missing-completion":
				p.RetirementCompletion = nil
			case "missing-finalization":
				p.RetirementFinalization = nil
			case "null":
				p.RetirementFinalization = json.RawMessage(`null`)
			case "array":
				p.RetirementCompletion = json.RawMessage(`[]`)
			case "trailing":
				p.Retirement = json.RawMessage(`{} {}`)
			case "blank-owner":
				uid = " "
			}
			v.Released = map[string]ReceiveJournalPublication{uid: p}
			err := v.validate()
			if fault == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid retired-owner record accepted")
			}
		})
	}
}

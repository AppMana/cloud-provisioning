package tunnel

import (
	"encoding/json"
	"testing"
)

func TestReceivePublicationPinsJournalIncarnation(t *testing.T) {
	for _, tc := range []struct {
		name           string
		publication    *ReceiveJournalPublication
		uid            string
		allowed, valid bool
	}{
		{"legacy", nil, "journal", true, true},
		{"pending", &ReceiveJournalPublication{Plan: json.RawMessage(`{}`)}, "journal", false, true},
		{"published", &ReceiveJournalPublication{JournalUID: "journal"}, "journal", true, true},
		{"replacement", &ReceiveJournalPublication{JournalUID: "journal"}, "replacement", false, true},
		{"empty", &ReceiveJournalPublication{}, "journal", false, false},
		{"ambiguous", &ReceiveJournalPublication{Plan: json.RawMessage(`{}`), JournalUID: "journal"}, "journal", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := ReceiveInventory{Version: 1, ClusterUID: "cluster", Namespace: "system", MeshName: "mesh", MeshUID: "mesh-uid", MachineUIDs: []string{"worker"}}
			if tc.publication != nil {
				v.Publications = map[string]ReceiveJournalPublication{"worker": *tc.publication}
			}
			if (v.validate() == nil) != tc.valid {
				t.Fatal("unexpected publication state validation")
			}
			if tc.valid && (v.CheckJournal("worker", tc.uid) == nil) != tc.allowed {
				t.Fatal("unexpected journal admission")
			}
			if v.CheckJournal("unknown", tc.uid) == nil {
				t.Fatal("unreserved journal admitted")
			}
		})
	}
}

package tunnel

import (
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// These checks exercise terminal authority and public identity, not native
// listener coexistence, crash recovery, packet continuity or route switching.
func TestReceivePreparationRequiresExactTerminalPredecessor(t *testing.T) {
	for _, outcome := range []string{"forward", "abort", "rollback"} {
		t.Run(outcome, func(t *testing.T) {
			plan, tr := journalRouteFixture(t, routeSetFixture(t)[0])
			initial := tr
			rolled := false
			for {
				view, err := tr.View()
				if err != nil {
					t.Fatal(err)
				}
				if view.Complete {
					break
				}
				at := view.Round.IssuedAt.Add(time.Second)
				if !rolled && ((outcome == "abort" && view.Round.Phase == handover.Prepare) || (outcome == "rollback" && view.Round.Phase == handover.Switch)) {
					tr, err = tr.Rollback(at)
					if err != nil {
						t.Fatal(err)
					}
					rolled = true
				} else {
					if view.Round.Phase == handover.Drain {
						at = view.DrainUntil
					}
					tr = advanceJournalRoute(t, tr, at)
				}
			}
			snapshot := journalRouteSnapshot(t, plan, tr)
			ref, err := NewReceivePreparePredecessor(snapshot, "cluster")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewReceivePreparePredecessor(journalRouteSnapshot(t, plan, initial), "cluster"); err == nil {
				t.Fatal("unfinished journal authorized successor preparation")
			}
			for _, fault := range []string{"none", "missing", "cluster", "machine", "journal", "history", "mesh", "node-name", "node-uid", "root", "base", "generation", "invalid-generation", "port", "unfinished"} {
				t.Run(fault, func(t *testing.T) {
					binding := ref
					req := ReceivePrepare{Version: 1, Predecessor: &binding, ReceiverGeneration: strings.Repeat("a", 64), MeshUID: plan.MeshUID, NodeName: plan.CloudNodeName, NodeUID: plan.CloudNodeUID, RootPublicKey: plan.RootPublicKey, BaseHash: HashPeerList(plan.Base), Ports: map[string]int{}}
					live := snapshot
					switch fault {
					case "missing":
						req.Predecessor = nil
					case "cluster":
						binding.ClusterUID = "other"
					case "machine":
						binding.MachineUID = "other"
					case "journal":
						binding.JournalUID = "recreated"
					case "history":
						binding.JournalSHA256 = strings.Repeat("0", 64)
					case "mesh":
						req.MeshUID = "other"
					case "node-name":
						req.NodeName = "other"
					case "node-uid":
						req.NodeUID = "other"
					case "root":
						req.RootPublicKey = receiverTestKey(99)
					case "base":
						req.BaseHash = "changed"
					case "generation":
						req.ReceiverGeneration = plan.ReceiverGeneration
					case "invalid-generation":
						req.ReceiverGeneration = "not-a-generation"
					case "port":
						for key, port := range plan.Ports {
							req.Ports[key] = port
						}
						if len(req.Ports) == 0 {
							t.Fatal("fixture omitted predecessor listeners")
						}
					case "unfinished":
						live = journalRouteSnapshot(t, plan, initial)
					}
					got, err := req.ValidatePredecessor(live)
					if fault == "none" {
						if err != nil || got.MachineUID != plan.MachineUID {
							t.Fatal("valid predecessor rejected", err)
						}
					} else if err == nil {
						t.Fatal("invalid predecessor admitted", fault)
					}
				})
			}
		})
	}
}

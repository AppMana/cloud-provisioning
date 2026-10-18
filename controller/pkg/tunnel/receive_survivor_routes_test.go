package tunnel

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
)

// Synthetic histories prove route projection and retained ownership only, not
// authenticated publication, native cleanup, reboot recovery or continuity.
func TestReceiveSurvivorRoutes(t *testing.T) {
	for _, mode := range []string{"forward", "abort", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			plans := routeSetFixture(t)
			retired, tr := journalRouteFixture(t, plans[0])
			survivor, live := journalRouteFixture(t, plans[1])
			if mode == "rollback" {
				tr = advanceJournalRoute(t, tr, journalRouteEpoch.Add(time.Second))
			}
			if mode != "forward" {
				var err error
				tr, err = tr.Rollback(journalRouteEpoch.Add(2 * time.Second))
				if err != nil {
					t.Fatal(err)
				}
			}
			for i := 3; ; i++ {
				v, err := tr.View()
				if err != nil {
					t.Fatal(err)
				}
				if v.Complete {
					break
				}
				if i > 15 {
					t.Fatal("history did not terminate")
				}
				at := journalRouteEpoch.Add(time.Duration(i) * time.Second)
				if !v.DrainUntil.IsZero() && at.Before(v.DrainUntil) {
					at = v.DrainUntil
				}
				tr = advanceJournalRoute(t, tr, at)
			}
			snapshot := journalRouteSnapshot(t, retired, tr)
			freeze := func(s handover.Snapshot) handover.Snapshot {
				raw, err := s.Checkpoint()
				if err != nil {
					t.Fatal(err)
				}
				var record map[string]any
				if err := json.Unmarshal(raw, &record); err != nil {
					t.Fatal(err)
				}
				record["object"].(map[string]any)["immutable"] = true
				raw, err = json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				got, err := (handover.Store{Name: ReceiveTransitionName(retired.MachineUID), ClusterUID: "cluster"}).RestoreCheckpoint(raw)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			frozen := freeze(snapshot)
			active := journalRouteSnapshot(t, survivor, live)
			withdrawal := map[string]string{retired.MachineUID: string(snapshot.Metadata().UID)}
			switched := journalRouteSnapshot(t, survivor, advanceJournalRoute(t, live, journalRouteEpoch.Add(time.Second)))
			for _, serving := range []handover.Snapshot{active, switched} {
				want, err := PlanReceiveJournalRoutes([]handover.Snapshot{serving}, "cluster")
				if err != nil {
					t.Fatal(err)
				}
				want.Withdrawn = map[string]string{retired.MachineUID: retired.RootPublicKey}
				for _, order := range [][]handover.Snapshot{{frozen, serving}, {serving, frozen}} {
					got, err := PlanReceiveSurvivorRoutes(order, "cluster", withdrawal)
					if err != nil {
						t.Fatal(err)
					}
					if len(got.Reserved.Plans) != 2 || !reflect.DeepEqual(got.Serving, want) {
						t.Fatal("lost ownership or changed survivor intent")
					}
				}
			}
			for _, fault := range []string{"unfrozen", "wrong-uid", "unknown", "empty-uid", "unfinished", "overlap"} {
				t.Run(fault, func(t *testing.T) {
					pair := []handover.Snapshot{frozen, active}
					remove := map[string]string{retired.MachineUID: string(snapshot.Metadata().UID)}
					switch fault {
					case "unfrozen":
						pair[0] = snapshot
					case "wrong-uid":
						remove[retired.MachineUID] = "other"
					case "unknown":
						remove["missing"] = "journal"
					case "empty-uid":
						remove[retired.MachineUID] = ""
					case "unfinished":
						_, initial := journalRouteFixture(t, plans[0])
						pair[0] = freeze(journalRouteSnapshot(t, retired, initial))
					case "overlap":
						conflict := plans[1]
						conflict.SiteDesired.Peers[0].WGAllowedIPs = append([]string(nil), plans[0].SiteDesired.Peers[0].WGAllowedIPs...)
						conflict.SiteDesired.Peers[0].RouteHosts = append([]string(nil), plans[0].SiteDesired.Peers[0].RouteHosts...)
						refreshRouteSetClients(t, &conflict)
						p, h := journalRouteFixture(t, conflict)
						pair[1] = journalRouteSnapshot(t, p, h)
					}
					if _, err := PlanReceiveSurvivorRoutes(pair, "cluster", remove); err == nil {
						t.Fatal("unsafe withdrawal projection accepted")
					}
				})
			}
			empty, err := PlanReceiveSurvivorRoutes([]handover.Snapshot{frozen}, "cluster", withdrawal)
			if err != nil || len(empty.Reserved.Plans) != 1 || len(empty.Serving.Plans) != 0 || len(empty.Serving.Receivers) != 0 {
				t.Fatal("last withdrawal released ownership or retained serving routes", err)
			}
		})
	}
}

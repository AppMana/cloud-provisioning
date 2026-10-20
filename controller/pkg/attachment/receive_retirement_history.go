package attachment

import (
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// Retained history proves only the identity and obligations originally consumed.
// It must not require current peer absence or current consumer membership: those
// belong to live mutation admission. Callers separately validate journal ancestry
// and pin every source revision before using the historical result.
func retainedReceiveWithdrawal(inventory tunnel.ReceiveInventory, machineUID string, snapshot handover.Snapshot, publication tunnel.ReceiveJournalPublication) (RemoteWithdrawalIntent, tunnel.ReceiveTransitionPlan, error) {
	fail := func(err error) (RemoteWithdrawalIntent, tunnel.ReceiveTransitionPlan, error) {
		return RemoteWithdrawalIntent{}, tunnel.ReceiveTransitionPlan{}, err
	}
	meta := snapshot.Metadata()
	if !snapshot.RetirementFrozen() || meta.UID == "" || meta.ResourceVersion == "" || meta.DeletionTimestamp != nil || publication.JournalUID != string(meta.UID) || len(publication.Plan) != 0 {
		return fail(fmt.Errorf("retained withdrawal requires its exact frozen journal"))
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, inventory.ClusterUID)
	if err != nil {
		return fail(err)
	}
	if plan.MachineUID != machineUID || plan.Namespace != inventory.Namespace || plan.MeshName != inventory.MeshName || plan.MeshUID != inventory.MeshUID {
		return fail(fmt.Errorf("retained journal scope changed"))
	}
	record, err := decodeReceiveWithdrawalRecord(publication.Retirement, machineUID)
	if err != nil {
		return fail(err)
	}
	history, err := snapshot.Transition().Bytes()
	if err != nil {
		return fail(err)
	}
	w := record.Withdrawal
	if err := w.validateJournalWithdrawalFence(snapshot); err != nil {
		return fail(err)
	}
	if record.JournalSHA256 != tunnel.HashPeerList(history) || w.MeshNamespace != plan.Namespace || w.MeshName != plan.MeshName || w.MeshUID != plan.MeshUID || w.MachineUID != machineUID || w.MachineName != plan.MachineName || w.PublicKey != plan.RootPublicKey || w.SourceVersion == "" || w.DrainIntent == "" {
		return fail(fmt.Errorf("retained withdrawal differs from journal"))
	}
	if w.Orphaned && (w.DrainIntent != "orphan:"+string(meta.UID) || w.SourceDataHash == "") {
		return fail(fmt.Errorf("retained orphan identity changed"))
	}
	if _, _, err := w.receiveWithdrawalConsumers(plan); err != nil {
		return fail(err)
	}
	return w, plan, nil
}

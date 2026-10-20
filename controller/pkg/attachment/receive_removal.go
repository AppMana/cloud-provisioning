package attachment

import (
	"context"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// AdvanceReceiveRemoval advances the shared receiver cleanup stages for a
// persisted drain owner. The caller supplies fresh lifecycle authorization and
// commits its own removal state only after ready is true. It never deletes
// compute, releases reservations or treats a missing journal as cleanup.
//
// expectedIndex pins discovery to the API inventory incarnation and revision.
// Every invocation reloads durable state; a lost response is resolved by the
// next invocation, not an in-memory stage counter. The callback must verify
// exact persisted intent, drained workload and live Machine/Node identities.
func AdvanceReceiveRemoval(ctx context.Context, api kubernetes.Interface, store handover.Store, expectedIndex metav1.ObjectMeta, mesh *corev1.Secret, intent RemoteWithdrawalIntent, nodeName, nodeUID string, maxAge time.Duration, authorize func(context.Context, RemoteWithdrawalIntent) error) (ready bool, err error) {
	if api == nil || store.ConfigMaps == nil || store.ClusterUID == "" || mesh == nil || authorize == nil || maxAge <= 0 || nodeName == "" || nodeUID == "" || expectedIndex.UID == "" || expectedIndex.ResourceVersion == "" {
		return false, fmt.Errorf("receiver removal requires exact discovery, lifecycle authority and receipt policy")
	}
	if mesh.Namespace != intent.MeshNamespace || mesh.Name != intent.MeshName || string(mesh.UID) != intent.MeshUID || mesh.DeletionTimestamp != nil || store.Name != tunnel.ReceiveTransitionName(intent.MachineUID) {
		return false, fmt.Errorf("receiver removal scope differs from captured intent")
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return false, err
	}
	if index.UID != expectedIndex.UID || index.ResourceVersion != expectedIndex.ResourceVersion {
		return false, fmt.Errorf("retirement discovery changed")
	}
	publication := inventory.Publications[intent.MachineUID]
	if publication.JournalUID == "" {
		return false, fmt.Errorf("receiver journal is not fully published")
	}
	if store.ExpectedUID != "" && store.ExpectedUID != publication.JournalUID {
		return false, fmt.Errorf("receiver journal discovery changed")
	}
	store.ExpectedUID = publication.JournalUID
	snapshot, err := store.Load(ctx)
	if err != nil {
		return false, err
	}
	if err := authorize(ctx, intent); err != nil {
		return false, err
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		return false, err
	}
	if plan.MachineUID != intent.MachineUID || plan.MachineName != intent.MachineName || plan.CloudNodeUID != nodeUID || plan.CloudNodeName != nodeName || plan.Namespace != mesh.Namespace || plan.MeshName != mesh.Name || plan.MeshUID != string(mesh.UID) || plan.RootPublicKey != intent.PublicKey {
		return false, fmt.Errorf("receiver journal differs from drain owner")
	}
	if !snapshot.Frozen() {
		view, err := snapshot.Transition().View()
		if err != nil {
			return false, err
		}
		// An executable transport retains its normal bounded retry. An expired
		// transport can no longer complete through fresh phase receipts; fence
		// its exact lifecycle intent without pretending that it completed.
		if !view.Complete {
			now := time.Now()
			if now.Before(view.Round.ExpiresAt) {
				return false, nil
			}
			// The fence is irreversible. Reject an incomplete or stale capture
			// before binding it, rather than leaving an unusable frozen journal.
			if intent.Orphaned || intent.DrainIntent == "" || intent.SourceVersion == "" ||
				string(mesh.Data[tunnel.PeerPublicKeyPrefix+intent.MachineName]) != intent.PublicKey {
				return false, fmt.Errorf("interrupted withdrawal requires a published drained owner")
			}
			if (intent.SourceDataHash != "" && intent.SourceDataHash != withdrawalSourceDataHash(mesh.Data)) ||
				(intent.SourceDataHash == "" && intent.SourceVersion != mesh.ResourceVersion) {
				return false, fmt.Errorf("interrupted withdrawal capture changed before freezing")
			}
			if _, _, err := intent.receiveWithdrawalConsumers(plan); err != nil {
				return false, err
			}
			digest, err := intent.withdrawalIntentHash()
			if err != nil {
				return false, err
			}
			if err := tunnel.ValidateReceiveTransportHistory(ctx, store, snapshot); err != nil {
				return false, err
			}
			if err := authorize(ctx, intent); err != nil {
				return false, err
			}
			_, err = store.FreezeExpiredWithdrawal(ctx, snapshot, digest, now)
			return false, err
		}
		_, err = store.FreezeCompleted(ctx, snapshot)
		return false, err
	}
	if len(publication.Retirement) == 0 {
		return false, intent.ReserveReceiveWithdrawal(ctx, store.ConfigMaps, store.ClusterUID, string(index.UID), mesh, snapshot)
	}
	if _, err := PublishReceiveWithdrawal(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), intent.MachineUID, string(index.UID), authorize); err != nil {
		return false, err
	}
	complete, err := VerifyReceiveRetirementCompletion(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), intent.MachineUID, string(index.UID), maxAge)
	if err != nil || !complete {
		return false, err
	}
	if err := authorize(ctx, intent); err != nil {
		return false, err
	}
	// Lifecycle checks can take time; revalidate age and incarnations afterwards.
	return VerifyReceiveRetirementCompletion(ctx, api, store, mesh.Namespace, mesh.Name, string(mesh.UID), intent.MachineUID, string(index.UID), maxAge)
}

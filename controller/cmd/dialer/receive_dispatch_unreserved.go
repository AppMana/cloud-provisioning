package main

import (
	"context"
	"fmt"
	"reflect"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Released history applies to its participants, not every subsequently added
// worker. This only selects the existing ordinary reconciler: its host fence,
// adoption validation and native checks still apply. Never erase local history
// or turn an API failure into permission to use bootstrap state.
func dispatchUnreservedReceiveCloud(ctx context.Context, api kubernetes.Interface, store handover.Store, cfg config, inventory tunnel.ReceiveInventory) (bool, error) {
	lock, err := lockLegacyReconciliation(cfg)
	if err != nil {
		return true, err
	}
	defer lock.Close()
	key, _, err := receiveDispatchIdentity(cfg)
	if err != nil {
		return true, err
	}
	node, err := api.CoreV1().Nodes().Get(ctx, cfg.nodeName, metav1.GetOptions{})
	if err != nil {
		return true, err
	}
	if node.UID == "" || node.DeletionTimestamp != nil {
		return true, fmt.Errorf("unreserved cloud dispatch requires a live node")
	}
	adoption, err := cloudAdoptionSecretName(cfg)
	if err != nil {
		return true, err
	}
	local := func(plan tunnel.ReceiveTransitionPlan, reserveNames bool) bool {
		if (reserveNames && plan.CloudNodeName == cfg.nodeName) || plan.CloudNodeUID == string(node.UID) || plan.RootPublicKey == key.PublicKey().String() || plan.AdoptionName == adoption {
			return true
		}
		for public, site := range plan.Sites {
			if (reserveNames && site.NodeName == cfg.nodeName) || site.NodeUID == string(node.UID) || public == key.PublicKey().String() {
				return true
			}
		}
		return false
	}
	current, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, inventory.Namespace, inventory.MeshName, inventory.MeshUID)
	if err != nil {
		return true, err
	}
	if !reflect.DeepEqual(current, inventory) {
		return true, fmt.Errorf("unreserved cloud inventory changed during discovery")
	}
	// Reload active membership as well: a worker may have acquired a reservation
	// after dispatch's first discovery, or have changed its name while retaining
	// a reserved key. Neither case may fall through to the ordinary writer.
	for _, uid := range current.MachineUIDs {
		member := store
		member.Name, member.ExpectedUID = tunnel.ReceiveTransitionName(uid), current.Publications[uid].JournalUID
		snapshot, err := member.Load(ctx)
		if err != nil {
			return true, err
		}
		if err := current.CheckJournal(uid, string(snapshot.Metadata().UID)); err != nil {
			return true, err
		}
		plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
		if err != nil {
			return true, err
		}
		if local(plan, true) {
			return true, nil
		}
		if len(current.Publications[uid].Retirement) != 0 {
			// Active withdrawals require the normal dispatcher to admit captured
			// consumers, including those without their own reserved journal.
			return true, nil
		}
	}
	// The existing shared reader validates every retained history and selects
	// each journal's frozen receipt age. Reconstructing that set separately for
	// each retired worker delays ordinary admission as release history grows.
	histories, err := attachment.ReadReleasedReceiveJournals(ctx, api, store, current.Namespace, current.MeshName, current.MeshUID, string(index.UID), 0)
	if err != nil {
		return true, err
	}
	if len(histories.Records) != len(current.Released) {
		return true, fmt.Errorf("unreserved cloud release membership changed")
	}
	for uid := range current.Released {
		history, verified := histories.Records[uid]
		if !verified || !sameReceiveRevision(index.ObjectMeta, history.Metadata) {
			return true, fmt.Errorf("unreserved cloud release history changed")
		}
		// Released names can belong to a new Node incarnation. Keep the
		// historical UID, key, adoption and durable-file fences; a name alone
		// must not strand a fresh worker before it acquires its own journal.
		if local(history.Plan, false) {
			return true, nil
		}
		for _, consumer := range history.Withdrawal.Consumers {
			if consumer.NodeUID == string(node.UID) || consumer.PublicKey == key.PublicKey().String() || (consumer.MachineName != "" && tunnel.AdoptionSecretName(consumer.MachineName) == adoption) {
				return true, nil
			}
		}
	}
	if len(histories.Records) != 0 {
		if err := histories.Recheck(ctx, api, store); err != nil {
			return true, err
		}
	}
	return false, ctx.Err()
}

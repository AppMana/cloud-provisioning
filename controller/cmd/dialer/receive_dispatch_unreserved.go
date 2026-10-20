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
	local := func(plan tunnel.ReceiveTransitionPlan) bool {
		if plan.CloudNodeName == cfg.nodeName || plan.CloudNodeUID == string(node.UID) || plan.RootPublicKey == key.PublicKey().String() || plan.AdoptionName == adoption {
			return true
		}
		for public, site := range plan.Sites {
			if site.NodeName == cfg.nodeName || site.NodeUID == string(node.UID) || public == key.PublicKey().String() {
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
		if local(plan) {
			return true, nil
		}
		if len(current.Publications[uid].Retirement) != 0 {
			// Active withdrawals require the normal dispatcher to admit captured
			// consumers, including those without their own reserved journal.
			return true, nil
		}
	}
	var histories []attachment.ReceiveReleasedJournal
	for uid, publication := range current.Released {
		member := store
		member.Name, member.ExpectedUID = tunnel.ReceiveTransitionName(uid), publication.JournalUID
		snapshot, err := member.Load(ctx)
		if err != nil {
			return true, err
		}
		view, err := snapshot.Transition().View()
		if err != nil {
			return true, err
		}
		history, verified, err := attachment.ReadReleasedReceiveJournal(ctx, api, member, current.Namespace, current.MeshName, current.MeshUID, uid, string(index.UID), view.Round.MaxReceiptAge)
		if err != nil {
			return true, err
		}
		if !verified || !sameReceiveRevision(index.ObjectMeta, history.Metadata) {
			return true, fmt.Errorf("unreserved cloud release history changed")
		}
		if local(history.Plan) {
			return true, nil
		}
		for _, consumer := range history.Withdrawal.Consumers {
			if consumer.NodeName == cfg.nodeName || consumer.NodeUID == string(node.UID) || consumer.PublicKey == key.PublicKey().String() || (consumer.MachineName != "" && tunnel.AdoptionSecretName(consumer.MachineName) == adoption) {
				return true, nil
			}
		}
		histories = append(histories, history)
	}
	for _, history := range histories {
		if err := history.Recheck(ctx, api, store); err != nil {
			return true, err
		}
	}
	return false, ctx.Err()
}

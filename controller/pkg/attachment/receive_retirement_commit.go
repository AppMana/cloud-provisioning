package attachment

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const receiveRetirementCompletionVersion = 3

type receiveRetirementCompletion struct {
	InventoryVersion string             `json:"inventoryVersion"`
	InventoryHash    string             `json:"inventoryHash"`
	MeshVersion      string             `json:"meshVersion"`
	Version          int                `json:"version"`
	Binding          string             `json:"binding"`
	Receipts         []handover.Receipt `json:"receipts"`
}

// RecordReceiveRetirementWithPodReceipts joins live authentication to inventory
// CAS. recheck must admit the controller's full journal/mesh/consumer source set;
// a per-worker receipt scope is not a substitute for that discovery. Source
// revisions are checked again after authentication, and no conflict is retried.
// Existing completion is refreshed only after a new complete authentication,
// against the exact prior record and inventory revision. Completion remains
// historical evidence, never automatic reservation release.
func RecordReceiveRetirementWithPodReceipts(ctx context.Context, api kubernetes.Interface, store handover.Store, inventoryUID, expectedDigest string, mesh *corev1.Secret, snapshot handover.Snapshot, submissions []handover.PodReceipt, policies map[string]handover.PodReceiptAuthenticator, maxAge time.Duration, recheck func() error) error {
	if api == nil || store.ConfigMaps == nil || mesh == nil || expectedDigest == "" || recheck == nil {
		return fmt.Errorf("retirement commit requires live admission and exact receipt binding")
	}
	checkSource := func() error {
		return recheckReceiveRetirementSource(ctx, api, store, mesh, snapshot, recheck)
	}
	if err := checkSource(); err != nil {
		return err
	}
	scope, err := NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, inventoryUID, snapshot, mesh)
	if err != nil {
		return err
	}
	if scope.Digest() != expectedDigest {
		return fmt.Errorf("retirement receipt binding changed before authentication")
	}
	proofs, err := scope.AuthenticateComplete(ctx, submissions, policies, maxAge)
	if err != nil {
		return err
	}
	if err := checkSource(); err != nil {
		return err
	}
	current, err := NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, inventoryUID, snapshot, mesh)
	if err != nil {
		return err
	}
	if current.Digest() != scope.Digest() {
		return fmt.Errorf("retirement source changed during authentication")
	}
	for _, proof := range proofs {
		policy := policies[proof.AuthenticatedNodeUID]
		if err := handover.VerifyObservationIncarnation(ctx, policy.API, policy.Namespace, scope.nodes[proof.AuthenticatedNodeUID], proof.Receipt); err != nil {
			return err
		}
	}
	// Rechecks may take time; credentials are not persisted and cannot extend
	// the age of the observations that the controller is about to record.
	if err := scope.validateSubmissions(submissions, time.Now(), maxAge); err != nil {
		return err
	}
	completion := receiveRetirementCompletion{Version: receiveRetirementCompletionVersion, InventoryVersion: scope.inventory.ResourceVersion, InventoryHash: scope.inventoryHash, MeshVersion: mesh.ResourceVersion, Binding: scope.Digest(), Receipts: make([]handover.Receipt, 0, len(proofs))}
	for _, proof := range proofs {
		completion.Receipts = append(completion.Receipts, proof.Receipt)
	}
	sort.Slice(completion.Receipts, func(i, j int) bool { return completion.Receipts[i].NodeUID < completion.Receipts[j].NodeUID })
	raw, err := json.Marshal(completion)
	if err != nil {
		return err
	}
	if len(scope.completion) > 0 {
		return tunnel.RefreshReceiveRetirementCompletion(ctx, store.ConfigMaps, store.ClusterUID, scope.inventory, mesh, snapshot, scope.intent, scope.completion, raw)
	}
	return tunnel.RecordReceiveRetirementCompletion(ctx, store.ConfigMaps, store.ClusterUID, scope.inventory, mesh, snapshot, scope.intent, raw)
}

func recheckReceiveRetirementSource(ctx context.Context, api kubernetes.Interface, store handover.Store, mesh *corev1.Secret, snapshot handover.Snapshot, recheck func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := recheck(); err != nil {
		return err
	}
	current, err := api.CoreV1().Secrets(mesh.Namespace).Get(ctx, mesh.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if current.UID != mesh.UID || current.ResourceVersion != mesh.ResourceVersion || current.DeletionTimestamp != nil {
		return fmt.Errorf("retirement mesh changed during authentication")
	}
	journal, err := store.Load(ctx)
	if err != nil {
		return err
	}
	return journal.ContinuesExecution(snapshot)
}

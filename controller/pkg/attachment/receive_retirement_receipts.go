package attachment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// ReceiveRetirementReceiptScope binds complete-consumer absence observations to
// a persisted withdrawal. It does not advance the frozen transport journal.
// Reconstruct from live state before authentication and recheck the same digest
// before any reservation release. Neither construction nor authentication proves
// kernel absence; each admitted agent must verify every cleanup obligation.
type ReceiveRetirementReceiptScope struct {
	digest    string
	nodes     map[string]string
	inventory metav1.ObjectMeta
	intent    json.RawMessage
}

func (s ReceiveRetirementReceiptScope) Digest() string { return s.digest }

func NewReceiveRetirementReceiptScope(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID, inventoryUID string, snapshot handover.Snapshot, mesh *corev1.Secret) (ReceiveRetirementReceiptScope, error) {
	fail := func(err error) (ReceiveRetirementReceiptScope, error) { return ReceiveRetirementReceiptScope{}, err }
	if maps == nil || mesh == nil || inventoryUID == "" {
		return fail(fmt.Errorf("retirement receipts require live inventory and mesh"))
	}
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, maps, clusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return fail(err)
	}
	if string(index.UID) != inventoryUID || index.ResourceVersion == "" || index.DeletionTimestamp != nil {
		return fail(fmt.Errorf("retirement receipt inventory changed"))
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, clusterUID)
	if err != nil {
		return fail(err)
	}
	withdrawal, err := ReadReceiveWithdrawal(inventory, plan.MachineUID, snapshot, mesh)
	if err != nil {
		return fail(err)
	}
	raw, err := json.Marshal(struct {
		Kind             string          `json:"kind"`
		ClusterUID       string          `json:"clusterUID"`
		InventoryUID     string          `json:"inventoryUID"`
		InventoryVersion string          `json:"inventoryVersion"`
		MeshUID          string          `json:"meshUID"`
		MeshVersion      string          `json:"meshVersion"`
		Withdrawal       json.RawMessage `json:"withdrawal"`
	}{"cldt/receive-retirement-observation/v1", clusterUID, inventoryUID, index.ResourceVersion, string(mesh.UID), mesh.ResourceVersion, inventory.Publications[plan.MachineUID].Retirement})
	if err != nil {
		return fail(err)
	}
	sum := sha256.Sum256(raw)
	scope := ReceiveRetirementReceiptScope{digest: hex.EncodeToString(sum[:]), nodes: map[string]string{}, inventory: *index.ObjectMeta.DeepCopy(), intent: append(json.RawMessage(nil), inventory.Publications[plan.MachineUID].Retirement...)}
	for _, consumer := range withdrawal.Consumers {
		scope.nodes[consumer.NodeUID] = consumer.NodeName
	}
	if len(scope.nodes) == 0 {
		return fail(fmt.Errorf("retirement has no retained consumers"))
	}
	return scope, nil
}

// AuthenticateComplete reuses the live pod/node/boot/image authenticator and
// existing transport envelope. RoundHash carries this domain-separated digest;
// phase key fields must be empty, so handover observations cannot be replayed as
// deletion completion. Policies and maxAge are trusted controller settings.
// Bearers remain request-local. The caller must re-admit source state afterward.
func (s ReceiveRetirementReceiptScope) AuthenticateComplete(ctx context.Context, submissions []handover.PodReceipt, policies map[string]handover.PodReceiptAuthenticator, maxAge time.Duration) ([]handover.AuthenticatedReceipt, error) {
	if err := s.validateSubmissions(submissions, time.Now(), maxAge); err != nil {
		return nil, err
	}
	for _, submission := range submissions {
		if _, ok := policies[submission.Receipt.NodeUID]; !ok {
			return nil, fmt.Errorf("retirement consumer has no trusted agent policy")
		}
	}
	proofs := make([]handover.AuthenticatedReceipt, 0, len(submissions))
	for _, submission := range submissions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		policy := policies[submission.Receipt.NodeUID]
		proof, err := policy.Authenticate(ctx, submission.BearerToken, submission.Receipt, submission.BootID)
		if err != nil {
			return nil, err
		}
		proofs = append(proofs, proof)
	}
	// An earlier consumer may reboot while a later consumer authenticates.
	// Reuse the existing incarnation verifier before returning the full batch.
	for _, proof := range proofs {
		policy := policies[proof.AuthenticatedNodeUID]
		if err := handover.VerifyObservationIncarnation(ctx, policy.API, policy.Namespace, s.nodes[proof.AuthenticatedNodeUID], proof.Receipt); err != nil {
			return nil, err
		}
	}
	// Slow API calls cannot extend the observation lifetime.
	if err := s.validateSubmissions(submissions, time.Now(), maxAge); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return proofs, nil
}

func (s ReceiveRetirementReceiptScope) validateSubmissions(submissions []handover.PodReceipt, now time.Time, maxAge time.Duration) error {
	if len(s.digest) != 64 || len(s.nodes) == 0 || maxAge <= 0 || len(submissions) != len(s.nodes) {
		return fmt.Errorf("retirement requires a complete current consumer batch")
	}
	seen := map[string]bool{}
	for _, entry := range submissions {
		r := entry.Receipt
		if s.nodes[r.NodeUID] == "" || seen[r.NodeUID] || entry.BootID == "" || entry.BearerToken == "" || r.AgentContainerID == "" || r.RoundHash != s.digest || r.DeviceID != "" || r.FromKey != "" || r.ToKey != "" || r.FromPeerKey != "" || r.ToPeerKey != "" {
			return fmt.Errorf("retirement observation does not bind its consumer and withdrawal")
		}
		if r.ObservedAt.IsZero() || r.ObservedAt.After(now) || now.Sub(r.ObservedAt) > maxAge {
			return fmt.Errorf("retirement observation is stale or in the future")
		}
		seen[r.NodeUID] = true
	}
	return nil
}

package attachment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// ErrRetirementObservationExpired requires a new observation, never acceptance
// of the expired evidence. Lifecycle callers may wait for refresh without
// increasing an error backoff beyond the evidence's freshness window.
var ErrRetirementObservationExpired = errors.New("retirement observation has expired")

// ReceiveRetirementReceiptScope binds complete-consumer absence observations to
// a persisted withdrawal. It does not advance the frozen transport journal.
// Reconstruct from live state before authentication and recheck the same digest
// before any reservation release. Neither construction nor authentication proves
// kernel absence; each admitted agent must verify every cleanup obligation.
type ReceiveRetirementReceiptScope struct {
	digest        string
	nodes         map[string]string
	inventory     metav1.ObjectMeta
	completion    json.RawMessage
	inventoryHash string
	intent        json.RawMessage
	version       int
	cohort        []string
}

func (s ReceiveRetirementReceiptScope) Digest() string               { return s.digest }
func (s ReceiveRetirementReceiptScope) CleanupMachineUIDs() []string { return slices.Clone(s.cohort) }
func (s ReceiveRetirementReceiptScope) RequiresConsumer(nodeUID string) bool {
	return s.nodes[nodeUID] != ""
}

func receiveJointRetirementDigest(base string, members []string) (string, error) {
	raw, err := json.Marshal(struct {
		Kind    string   `json:"kind"`
		Binding string   `json:"binding"`
		Members []string `json:"members"`
	}{"cldt/joint-retirement-observation/v1", base, members})
	if err != nil {
		return "", err
	}
	return tunnel.HashPeerList(raw), nil
}

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
	cohort, err := loadReceiptRetirementCohort(ctx, handover.Store{ConfigMaps: maps, ClusterUID: clusterUID}, ReceiveRetirementSet{Inventory: inventory, Mesh: mesh, Withdrawal: withdrawal, Journals: map[string]handover.Snapshot{plan.MachineUID: snapshot}})
	if err != nil {
		return fail(err)
	}
	return receiveRetirementReceiptScope(inventory, index.ObjectMeta, mesh, plan.MachineUID, cohort)
}

// ReceiptScope binds this request's fully admitted source without replaying its
// histories. Obtain the set through ReadReceiveRetirementSet and never reuse it
// across requests. Callers must recheck live sources at execution and evidence
// admission boundaries; binding itself performs no API calls.
func (s ReceiveRetirementSet) ReceiptScope(ctx context.Context, store handover.Store) (ReceiveRetirementReceiptScope, error) {
	fail := func(err error) (ReceiveRetirementReceiptScope, error) { return ReceiveRetirementReceiptScope{}, err }
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	uid := s.Withdrawal.MachineUID
	snapshot, found := s.Journals[uid]
	if store.ConfigMaps == nil || s.Mesh == nil || !found || store.Name != tunnel.ReceiveTransitionName(uid) || (store.ExpectedUID != "" && store.ExpectedUID != string(snapshot.Metadata().UID)) || s.Inventory.ClusterUID != store.ClusterUID {
		return fail(fmt.Errorf("retirement receipt journal scope changed"))
	}
	if s.Metadata.UID == "" || s.MeshMetadata.UID != s.Mesh.UID || s.MeshMetadata.ResourceVersion != s.Mesh.ResourceVersion || snapshot.Metadata().Namespace != s.Mesh.Namespace || len(s.Journals) != len(s.Inventory.MachineUIDs) {
		return fail(fmt.Errorf("retirement receipt source scope changed"))
	}
	for _, member := range s.Inventory.MachineUIDs {
		journal, ok := s.Journals[member]
		if !ok {
			return fail(fmt.Errorf("retirement receipt omitted reserved journal"))
		}
		if err := s.Inventory.CheckJournal(member, string(journal.Metadata().UID)); err != nil {
			return fail(err)
		}
	}
	withdrawal, err := ReadReceiveWithdrawal(s.Inventory, uid, snapshot, s.Mesh)
	if err != nil {
		return fail(err)
	}
	if !reflect.DeepEqual(withdrawal, s.Withdrawal) {
		return fail(fmt.Errorf("retirement receipt withdrawal changed"))
	}
	cohort, err := s.RetirementCohort()
	if err != nil {
		return fail(err)
	}
	scope, err := receiveRetirementReceiptScope(s.Inventory, s.Metadata, s.Mesh, uid, cohort)
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return scope, nil
}

func receiveRetirementReceiptScope(inventory tunnel.ReceiveInventory, index metav1.ObjectMeta, mesh *corev1.Secret, machineUID string, cohort ReceiveRetirementCohort) (ReceiveRetirementReceiptScope, error) {
	fail := func(err error) (ReceiveRetirementReceiptScope, error) { return ReceiveRetirementReceiptScope{}, err }
	publication := inventory.Publications[machineUID]
	previousCompletion := append(json.RawMessage(nil), publication.RetirementCompletion...)
	version := receiveRetirementCompletionVersion
	if len(cohort.MachineUIDs) > 1 {
		version = receiveRetirementCohortCompletionVersion
	}
	inventoryHash, err := receiveRetirementInventoryHash(inventory, machineUID, version)
	if err != nil {
		return fail(err)
	}
	digest, err := receiveRetirementAuthorityDigest(inventory.ClusterUID, string(index.UID), inventoryHash, index, mesh, publication.Retirement)
	if err != nil {
		return fail(err)
	}
	if len(cohort.MachineUIDs) > 1 {
		digest, err = receiveJointRetirementDigest(digest, cohort.MachineUIDs)
		if err != nil {
			return fail(err)
		}
	}
	scope := ReceiveRetirementReceiptScope{version: version, cohort: cohort.MachineUIDs, digest: digest, inventoryHash: inventoryHash, completion: previousCompletion, nodes: map[string]string{}, inventory: *index.DeepCopy(), intent: append(json.RawMessage(nil), inventory.Publications[machineUID].Retirement...)}
	for _, consumer := range cohort.Consumers {
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
	return s.authenticate(ctx, submissions, policies, maxAge, true)
}

// AuthenticatePartial authenticates independently submitted consumer observations.
// It never authorizes completion; the collector must authenticate the complete
// batch again against current source state before recording completion.
func (s ReceiveRetirementReceiptScope) AuthenticatePartial(ctx context.Context, submissions []handover.PodReceipt, policies map[string]handover.PodReceiptAuthenticator, maxAge time.Duration) ([]handover.AuthenticatedReceipt, error) {
	return s.authenticate(ctx, submissions, policies, maxAge, false)
}

func (s ReceiveRetirementReceiptScope) authenticate(ctx context.Context, submissions []handover.PodReceipt, policies map[string]handover.PodReceiptAuthenticator, maxAge time.Duration, complete bool) ([]handover.AuthenticatedReceipt, error) {
	if err := s.validateBatch(submissions, time.Now(), maxAge, complete); err != nil {
		return nil, err
	}
	for _, submission := range submissions {
		if _, ok := policies[submission.Receipt.NodeUID]; !ok {
			return nil, fmt.Errorf("retirement consumer has no trusted agent policy")
		}
	}
	// Independent consumers authenticate concurrently under the caller's one
	// deadline. Bound API pressure and join every read before returning an error;
	// no partial proof set can authorize completion.
	proofs := make([]handover.AuthenticatedReceipt, len(submissions))
	group, authCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for i, submission := range submissions {
		group.Go(func() error {
			if err := authCtx.Err(); err != nil {
				return err
			}
			policy := policies[submission.Receipt.NodeUID]
			proof, err := policy.Authenticate(authCtx, submission.BearerToken, submission.Receipt, submission.BootID)
			if err != nil {
				return err
			}
			proofs[i] = proof
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		if cancelled := ctx.Err(); cancelled != nil {
			return nil, cancelled
		}
		return nil, err
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
	if err := s.validateBatch(submissions, time.Now(), maxAge, complete); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return proofs, nil
}

func (s ReceiveRetirementReceiptScope) validateSubmissions(submissions []handover.PodReceipt, now time.Time, maxAge time.Duration) error {
	return s.validateBatch(submissions, now, maxAge, true)
}

func (s ReceiveRetirementReceiptScope) validateBatch(submissions []handover.PodReceipt, now time.Time, maxAge time.Duration, complete bool) error {
	if len(s.digest) != 64 || len(s.nodes) == 0 || maxAge <= 0 || len(submissions) == 0 || len(submissions) > len(s.nodes) || (complete && len(submissions) != len(s.nodes)) {
		return fmt.Errorf("retirement requires a complete current consumer batch")
	}
	seen := map[string]bool{}
	for _, entry := range submissions {
		r := entry.Receipt
		if seen[r.NodeUID] || entry.BootID == "" || entry.BearerToken == "" {
			return fmt.Errorf("retirement observation lacks unique consumer credentials")
		}
		if err := s.validateObservation(r, now, maxAge); err != nil {
			return err
		}
		seen[r.NodeUID] = true
	}
	return nil
}

// The legacy binding remains the completion's exact source-revision binding.
func receiveRetirementDigest(clusterUID, inventoryUID, inventoryVersion string, mesh *corev1.Secret, intent json.RawMessage) (string, error) {
	raw, err := json.Marshal(struct {
		Kind             string          `json:"kind"`
		ClusterUID       string          `json:"clusterUID"`
		InventoryUID     string          `json:"inventoryUID"`
		InventoryVersion string          `json:"inventoryVersion"`
		MeshUID          string          `json:"meshUID"`
		MeshVersion      string          `json:"meshVersion"`
		Withdrawal       json.RawMessage `json:"withdrawal"`
	}{"cldt/receive-retirement-observation/v1", clusterUID, inventoryUID, inventoryVersion, string(mesh.UID), mesh.ResourceVersion, intent})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Receipt collection binds withdrawal authority, not another consumer's
// completion write. Keep inventory metadata other than server bookkeeping,
// all authority data, and the exact mesh revision. Commit still pins the live
// inventory revision independently; no write conflict is retried with receipts.
func receiveRetirementAuthorityDigest(clusterUID, inventoryUID, inventoryHash string, metadata metav1.ObjectMeta, mesh *corev1.Secret, intent json.RawMessage) (string, error) {
	metadata.ResourceVersion = ""
	metadata.ManagedFields = nil
	raw, err := json.Marshal(struct {
		Kind              string            `json:"kind"`
		ClusterUID        string            `json:"clusterUID"`
		InventoryUID      string            `json:"inventoryUID"`
		InventoryHash     string            `json:"inventoryHash"`
		InventoryMetadata metav1.ObjectMeta `json:"inventoryMetadata"`
		MeshUID           string            `json:"meshUID"`
		MeshVersion       string            `json:"meshVersion"`
		Withdrawal        json.RawMessage   `json:"withdrawal"`
	}{"cldt/receive-retirement-observation/v2", clusterUID, inventoryUID, inventoryHash, metadata, string(mesh.UID), mesh.ResourceVersion, intent})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s ReceiveRetirementReceiptScope) validateObservation(r handover.Receipt, now time.Time, maxAge time.Duration) error {
	if s.nodes[r.NodeUID] == "" || r.AgentContainerID == "" || r.RoundHash != s.digest || r.DeviceID != "" || r.FromKey != "" || r.ToKey != "" || r.FromPeerKey != "" || r.ToPeerKey != "" {
		return fmt.Errorf("retirement observation does not bind its consumer and withdrawal")
	}
	if maxAge <= 0 || r.ObservedAt.IsZero() || r.ObservedAt.After(now) {
		return fmt.Errorf("retirement observation is stale or in the future")
	}
	if now.Sub(r.ObservedAt) > maxAge {
		return ErrRetirementObservationExpired
	}
	return nil
}

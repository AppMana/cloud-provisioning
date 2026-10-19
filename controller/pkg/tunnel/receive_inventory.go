package tunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

const receiveInventoryKey = "inventory.json"

// ReceiveInventory is controller-owned discovery, not native completion. Entries
// are reserved before journal creation and cannot be removed by ordinary
// reconciliation. A missing reserved journal must never be treated as absence of
// handover intent. Agents need named read access, never write or list permission.
type ReceiveInventory struct {
	Version      int                                  `json:"version"`
	ClusterUID   string                               `json:"clusterUID"`
	Namespace    string                               `json:"namespace"`
	MeshName     string                               `json:"meshName"`
	MeshUID      string                               `json:"meshUID"`
	MachineUIDs  []string                             `json:"machineUIDs"`
	Publications map[string]ReceiveJournalPublication `json:"publications,omitempty"`
	// Released retains permanent retired-owner records outside active routing
	// reservations. Only the conditional release protocol may move a finalized
	// publication here. A released Machine UID can never be reserved again.
	Released map[string]ReceiveJournalPublication `json:"released,omitempty"`
}

// ReceiveJournalPublication stores either the prepared public plan or the
// published journal UID, never both. Pending plans grant no mutation authority.
// Inventory written before publication tracking remains readable, but missing
// legacy journals have no recoverable seed and must not be recreated.
type ReceiveJournalPublication struct {
	Plan       json.RawMessage `json:"plan,omitempty"`
	JournalUID string          `json:"journalUID,omitempty"`
	// Retirement retains the controller's withdrawal intent, not an agent
	// acknowledgement. Its interpretation belongs to the withdrawal owner;
	// ordinary journal publication and reservation must preserve it verbatim.
	Retirement json.RawMessage `json:"retirement,omitempty"`
	// Completion is controller-verified observation evidence, not permission
	// to release this reservation or skip fresh identity checks on reuse.
	RetirementCompletion json.RawMessage `json:"retirementCompletion,omitempty"`
	// Finalization consumes the exact completion record without releasing
	// ownership. Only the dedicated finalization writer may install it; later
	// completion refreshes cannot replace its consumed evidence. Readers that
	// do not recognize this field must fail closed.
	RetirementFinalization json.RawMessage `json:"retirementFinalization,omitempty"`
}

// CheckJournal admits old inventory entries as before, but new publications
// require the exact committed API incarnation. Pending plans are not executable.
func (v ReceiveInventory) CheckJournal(machineUID, journalUID string) error {
	if !slices.Contains(v.MachineUIDs, machineUID) {
		return fmt.Errorf("journal is not reserved")
	}
	if p, found := v.Publications[machineUID]; found && (p.JournalUID == "" || p.JournalUID != journalUID) {
		return fmt.Errorf("journal publication is pending or its incarnation changed")
	}
	return nil
}

func ReceiveInventoryName(meshUID string) string {
	sum := sha256.Sum256([]byte(meshUID))
	return fmt.Sprintf("receive-inventory-%x", sum[:16])
}

// Validate checks inventory structure only. It does not authenticate retirement
// evidence or authorize native restoration, release, or address reuse.
func (v ReceiveInventory) Validate() error { return v.validate() }

func (v ReceiveInventory) validate() error {
	if v.Version != 1 || v.ClusterUID == "" || v.Namespace == "" || v.MeshName == "" || v.MeshUID == "" || v.MachineUIDs == nil {
		return fmt.Errorf("receiver inventory requires complete mesh and cluster identity")
	}
	previous := ""
	for _, uid := range v.MachineUIDs {
		if uid == "" || strings.TrimSpace(uid) != uid || uid <= previous {
			return fmt.Errorf("receiver inventory machine identities must be unique and sorted")
		}
		previous = uid
	}
	for uid, p := range v.Publications {
		if !slices.Contains(v.MachineUIDs, uid) || (len(p.Plan) == 0) == (p.JournalUID == "") {
			return fmt.Errorf("invalid journal publication state")
		}
		if len(p.RetirementCompletion) > 0 && (len(p.Retirement) == 0 || !json.Valid(p.RetirementCompletion) || bytes.TrimSpace(p.RetirementCompletion)[0] != '{') {
			return fmt.Errorf("retirement completion requires persisted withdrawal and non-null evidence")
		}
		if len(p.RetirementFinalization) > 0 && (len(p.RetirementCompletion) == 0 || !json.Valid(p.RetirementFinalization) || bytes.TrimSpace(p.RetirementFinalization)[0] != '{') {
			return fmt.Errorf("retirement finalization requires retained completion and non-null evidence")
		}
		if len(p.Retirement) > 0 && (p.JournalUID == "" || !json.Valid(p.Retirement) || bytes.Equal(bytes.TrimSpace(p.Retirement), []byte("null"))) {
			return fmt.Errorf("retirement requires a published journal and non-null intent")
		}
	}
	for uid, p := range v.Released {
		if uid == "" || strings.TrimSpace(uid) != uid || slices.Contains(v.MachineUIDs, uid) {
			return fmt.Errorf("released journal overlaps active ownership or lacks identity")
		}
		if _, present := v.Publications[uid]; present {
			return fmt.Errorf("released journal remains actively published")
		}
		if p.JournalUID == "" || len(p.Plan) != 0 || len(p.RetirementFinalization) == 0 {
			return fmt.Errorf("released journal requires finalized publication")
		}
		for _, record := range []json.RawMessage{p.Retirement, p.RetirementCompletion, p.RetirementFinalization} {
			if !json.Valid(record) || bytes.TrimSpace(record)[0] != '{' {
				return fmt.Errorf("released journal requires retained object evidence")
			}
		}
	}
	return nil
}

// ReadReceiveInventory requires the expected mesh incarnation from admitted
// configuration. Kubernetes resource versions remain opaque; callers re-read
// this exact object after loading journals to detect inventory changes.
func ReadReceiveInventory(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID, namespace, meshName, meshUID string) (ReceiveInventory, *corev1.ConfigMap, error) {
	fail := func(err error) (ReceiveInventory, *corev1.ConfigMap, error) { return ReceiveInventory{}, nil, err }
	expected := ReceiveInventory{Version: 1, ClusterUID: clusterUID, Namespace: namespace, MeshName: meshName, MeshUID: meshUID, MachineUIDs: []string{}}
	if err := expected.validate(); err != nil {
		return fail(err)
	}
	if maps == nil {
		return fail(fmt.Errorf("receiver inventory requires API storage"))
	}
	object, err := maps.Get(ctx, ReceiveInventoryName(meshUID), metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if object.Name != ReceiveInventoryName(meshUID) || object.Namespace != namespace || object.UID == "" || object.ResourceVersion == "" || object.DeletionTimestamp != nil {
		return fail(fmt.Errorf("receiver inventory identity changed or is deleting"))
	}
	var inventory ReceiveInventory
	decoder := json.NewDecoder(bytes.NewBufferString(object.Data[receiveInventoryKey]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inventory); err != nil {
		return fail(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fail(fmt.Errorf("trailing receiver inventory data"))
	}
	if err := inventory.validate(); err != nil {
		return fail(err)
	}
	if inventory.ClusterUID != clusterUID || inventory.Namespace != namespace || inventory.MeshName != meshName || inventory.MeshUID != meshUID {
		return fail(fmt.Errorf("receiver inventory belongs to another mesh or cluster"))
	}
	return inventory, object, nil
}

// ReserveReceiveJournal only appends immutable Machine identities. Publication
// precedes journal creation, so a crash cannot leave an executable journal out
// of discovery. Concurrent updates return the API conflict for reconciliation to
// reload; they never overwrite another reservation. Cleanup requires a separate
// protocol after every participant has durably retired its references.
func ReserveReceiveJournal(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID string, mesh *corev1.Secret, machineUID string, owners []metav1.OwnerReference) error {
	return reserveReceiveJournal(ctx, maps, clusterUID, mesh, machineUID, owners, nil)
}

func ReservePreparedReceiveJournal(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID string, mesh *corev1.Secret, machineUID string, owners []metav1.OwnerReference, plan []byte) error {
	if len(plan) == 0 {
		return fmt.Errorf("prepared journal requires a public plan")
	}
	return reserveReceiveJournal(ctx, maps, clusterUID, mesh, machineUID, owners, plan)
}

func reserveReceiveJournal(ctx context.Context, maps typedcore.ConfigMapInterface, clusterUID string, mesh *corev1.Secret, machineUID string, owners []metav1.OwnerReference, plan []byte) error {
	if mesh == nil || mesh.UID == "" || mesh.ResourceVersion == "" || mesh.DeletionTimestamp != nil || machineUID == "" || strings.TrimSpace(machineUID) != machineUID || maps == nil {
		return fmt.Errorf("receiver reservation requires live identities and API storage")
	}
	inventory, object, err := ReadReceiveInventory(ctx, maps, clusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	creating := apierrors.IsNotFound(err)
	if err != nil && !creating {
		return err
	}
	if creating {
		// Absence is only a first-use condition when no journal for this mesh
		// exists. Recreating a deleted index from today's Machine list could
		// silently drop retired Machines whose routes are still in use.
		objects, err := maps.List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		for _, object := range objects.Items {
			if !strings.HasPrefix(object.Name, "receive-transition-") {
				continue
			}
			store := handover.Store{ConfigMaps: maps, Name: object.Name, ClusterUID: clusterUID}
			snapshot, err := store.Load(ctx)
			if err != nil {
				return fmt.Errorf("cannot establish first-use receiver inventory: %w", err)
			}
			plan, err := DecodeReceiveTransition(snapshot, clusterUID)
			if err != nil {
				return err
			}
			if plan.MeshUID == string(mesh.UID) {
				return fmt.Errorf("receiver inventory is missing for an existing journal")
			}
		}
		inventory = ReceiveInventory{Version: 1, ClusterUID: clusterUID, Namespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineUIDs: []string{}}
		object = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ReceiveInventoryName(string(mesh.UID)), Namespace: mesh.Namespace, OwnerReferences: owners}}
	}
	if _, released := inventory.Released[machineUID]; released {
		return fmt.Errorf("released Machine identity cannot be reserved again")
	}
	if slices.Contains(inventory.MachineUIDs, machineUID) {
		if len(plan) > 0 {
			p, found := inventory.Publications[machineUID]
			if !found || p.JournalUID != "" || !bytes.Equal(p.Plan, plan) {
				return fmt.Errorf("journal reservation already exists with different publication state")
			}
		}
		return nil
	}
	if len(plan) > 0 {
		if inventory.Publications == nil {
			inventory.Publications = map[string]ReceiveJournalPublication{}
		}
		inventory.Publications[machineUID] = ReceiveJournalPublication{Plan: bytes.Clone(plan)}
	}
	inventory.MachineUIDs = append(inventory.MachineUIDs, machineUID)
	slices.Sort(inventory.MachineUIDs)
	if err := inventory.validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	if object.Data == nil {
		object.Data = map[string]string{}
	}
	object.Data[receiveInventoryKey] = string(raw)
	if creating {
		_, err = maps.Create(ctx, object, metav1.CreateOptions{})
	} else {
		_, err = maps.Update(ctx, object, metav1.UpdateOptions{})
	}
	return err
}

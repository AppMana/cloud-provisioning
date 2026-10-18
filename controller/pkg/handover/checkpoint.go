package handover

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
)

// A checkpoint is trusted local recovery state, not a signed journal or an API
// write request. Keep it on the agent's private durable identity mount. Never
// accept checkpoints from readiness writers or use them to publish receipts.
type checkpoint struct {
	Version int               `json:"version"`
	Object  *corev1.ConfigMap `json:"object"`
}

func (s Snapshot) Checkpoint() ([]byte, error) {
	if s.object == nil {
		return nil, fmt.Errorf("missing journal snapshot")
	}
	if _, err := s.transition.View(); err != nil {
		return nil, err
	}
	return json.Marshal(checkpoint{Version: 1, Object: s.object})
}

// RestoreCheckpoint reuses storage identity, transition replay and plan-hash
// validation without API access. It does not reauthenticate participants or
// authorize a phase. ClusterUID and Name must come from admitted local scope.
func (s Store) RestoreCheckpoint(raw []byte) (Snapshot, error) {
	if !token(s.Name) || !token(s.ClusterUID) {
		return Snapshot{}, fmt.Errorf("invalid checkpoint scope")
	}
	var saved checkpoint
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&saved); err != nil {
		return Snapshot{}, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return Snapshot{}, fmt.Errorf("trailing checkpoint data")
	}
	if saved.Version != 1 || saved.Object == nil || saved.Object.DeletionTimestamp != nil {
		return Snapshot{}, fmt.Errorf("invalid or deleting checkpoint")
	}
	return s.snapshot(saved.Object)
}

// Extends checks an observation against the last durable journal, without
// ordering Kubernetes' opaque resource versions. A metadata-only update is
// allowed; object replacement, plan changes, regression and forks are not.
func (s Snapshot) Extends(previous Snapshot) error {
	if s.object == nil || previous.object == nil {
		return fmt.Errorf("missing journal snapshot")
	}
	a, b := s.object, previous.object
	if previous.Frozen() && !s.Frozen() {
		return fmt.Errorf("journal lost its durable write fence")
	}
	if a.UID != b.UID || a.Namespace != b.Namespace || a.Name != b.Name || a.DeletionTimestamp != nil || b.DeletionTimestamp != nil || !bytes.Equal(s.Plan(), previous.Plan()) {
		return fmt.Errorf("journal identity or public plan changed")
	}
	return s.transition.Extends(previous.transition)
}

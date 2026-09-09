package attachment

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProjectionStore changes only the gateway projection field. SecretUID binds
// publication to the original mesh; recreating a Secret requires explicit
// recovery rather than adopting a different mesh by name.
type ProjectionStore struct {
	Client                     client.Client
	Namespace, Name, SecretUID string
}

// RetireWorker durably changes only the retiring worker's render. Other
// recipients keep forwarding until that worker acknowledges its fallback.
// An absent projection is already globally withdrawn and must not be restored.
func (s ProjectionStore) RetireWorker(ctx context.Context, lease string) (*corev1.Secret, bool, error) {
	if s.Client == nil || s.Namespace == "" || s.Name == "" || s.SecretUID == "" || lease == "" {
		return nil, false, fmt.Errorf("projection client, mesh identity and lease required")
	}
	mesh := &corev1.Secret{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Name}, mesh); err != nil {
		return nil, false, err
	}
	if string(mesh.UID) != s.SecretUID || mesh.DeletionTimestamp != nil {
		return nil, false, fmt.Errorf("mesh Secret replaced or deleting")
	}
	var plans []tunnel.GatewayProjection
	if raw := mesh.Data[tunnel.GatewayProjectionsKey]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &plans); err != nil {
			return nil, false, err
		}
	}
	for _, plan := range plans {
		if plan.Lease != lease {
			continue
		}
		plan.RetiringWorker = true
		changed, err := setProjection(mesh, lease, &plan)
		if err != nil {
			return nil, false, err
		}
		if changed {
			if err := s.Client.Update(ctx, mesh); err != nil {
				return nil, false, err
			}
		}
		return mesh, true, nil
	}
	return mesh, false, nil
}

// Set returns the exact committed snapshot for consumer acknowledgement checks.
// A nil projection withdraws only lease. This is desired-state publication, not
// evidence that consumers applied it; callers must retain attachment finalizers.
func (s ProjectionStore) Set(ctx context.Context, lease string, projection *tunnel.GatewayProjection) (*corev1.Secret, error) {
	if s.Client == nil || s.Namespace == "" || s.Name == "" || s.SecretUID == "" || lease == "" {
		return nil, fmt.Errorf("projection client, mesh identity and lease required")
	}
	secret := &corev1.Secret{}
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Name}, secret); err != nil {
		return nil, err
	}
	if string(secret.UID) != s.SecretUID || secret.DeletionTimestamp != nil {
		return nil, fmt.Errorf("mesh Secret replaced or deleting")
	}
	changed, err := setProjection(secret, lease, projection)
	if err != nil {
		return nil, err
	}
	if changed {
		if err := s.Client.Update(ctx, secret); err != nil {
			return nil, err
		}
	}
	return secret, nil
}
func setProjection(secret *corev1.Secret, lease string, want *tunnel.GatewayProjection) (bool, error) {
	var plans []tunnel.GatewayProjection
	if raw := secret.Data[tunnel.GatewayProjectionsKey]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &plans); err != nil {
			return false, err
		}
	}
	if want != nil {
		if want.Lease != lease {
			return false, fmt.Errorf("projection lease mismatch")
		}
		counts := map[string]int{}
		for key, value := range secret.Data {
			if len(key) >= len(tunnel.PeerPublicKeyPrefix) && key[:len(tunnel.PeerPublicKeyPrefix)] == tunnel.PeerPublicKeyPrefix {
				counts[string(value)]++
			}
		}
		if counts[want.WorkerKey] != 1 || counts[want.GatewayKey] != 1 {
			return false, fmt.Errorf("projection keys are not uniquely published machine identities")
		}
	}
	next := make([]tunnel.GatewayProjection, 0, len(plans)+1)
	found := false
	for _, p := range plans {
		if p.Lease != lease {
			next = append(next, p)
			continue
		}
		if found {
			return false, fmt.Errorf("duplicate stored lease")
		}
		found = true
		if want != nil {
			allowed := p
			if want.RetiringWorker {
				allowed.RetiringWorker = true
			}
			if !reflect.DeepEqual(allowed, *want) {
				return false, fmt.Errorf("active projection is immutable; withdraw before replacement")
			}
			next = append(next, *want)
		}
	}
	if want != nil && !found {
		next = append(next, *want)
	}
	if want == nil && !found {
		return false, nil
	}
	sort.Slice(next, func(i, j int) bool { return next[i].Lease < next[j].Lease })
	raw, err := json.Marshal(next)
	if err != nil {
		return false, err
	}
	candidate := secret.DeepCopy()
	if candidate.Data == nil {
		candidate.Data = map[string][]byte{}
	}
	candidate.Data[tunnel.GatewayProjectionsKey] = raw
	// Validate the complete site render, including unrelated peer ownership,
	// before publishing any partial change to the shared document.
	if _, err = tunnel.SitePeers(candidate.Data); err != nil {
		return false, err
	}
	if string(secret.Data[tunnel.GatewayProjectionsKey]) == string(raw) {
		return false, nil
	}
	secret.Data = candidate.Data
	return true, nil
}

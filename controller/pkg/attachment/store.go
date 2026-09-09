package attachment

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const leaseFinalizer = "cloud-provisioning.appmana.com/network-attachment"

// ConfigMapStore retains retirement intent through controller restarts and
// deletion requests. Complete removes its finalizer; an active ConfigMap must
// not disappear before consumer withdrawal and provider release finish.
type ConfigMapStore struct {
	Client    client.Client
	Namespace string
}

func (s ConfigMapStore) key(id string) client.ObjectKey {
	sum := sha256.Sum256([]byte(id))
	return client.ObjectKey{Namespace: s.Namespace, Name: fmt.Sprintf("network-attachment-%x", sum[:16])}
}

func version(cm *corev1.ConfigMap) string { return string(cm.UID) + "/" + cm.ResourceVersion }

func (s ConfigMapStore) Load(ctx context.Context, id string) (*Record, error) {
	if s.Client == nil || s.Namespace == "" || id == "" {
		return nil, fmt.Errorf("store client, namespace and attachment ID are required")
	}
	cm := &corev1.ConfigMap{}
	if err := s.Client.Get(ctx, s.key(id), cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var record Record
	if err := json.Unmarshal([]byte(cm.Data["record.json"]), &record); err != nil {
		return nil, fmt.Errorf("decode attachment intent: %w", err)
	}
	if record.ID != id {
		return nil, fmt.Errorf("attachment ConfigMap identity mismatch")
	}
	record.Version = version(cm)
	record.Deleting = cm.DeletionTimestamp != nil
	return &record, nil
}

func (s ConfigMapStore) Save(ctx context.Context, previous, next *Record) error {
	if s.Client == nil || s.Namespace == "" || next == nil || next.ID == "" {
		return fmt.Errorf("store client, namespace and next attachment are required")
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	key := s.key(next.ID)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if previous != nil {
		if previous.ID != next.ID {
			return fmt.Errorf("attachment identity cannot change")
		}
		if err := s.Client.Get(ctx, key, cm); err != nil {
			return err
		}
		if version(cm) != previous.Version {
			return fmt.Errorf("attachment intent changed concurrently")
		}
	}
	if cm.DeletionTimestamp != nil && next.Phase != Withdrawing && next.Phase != Releasing && next.Phase != Complete {
		return fmt.Errorf("attachment intent is deleting; withdraw before preparing another lease")
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["record.json"] = string(raw)
	if next.Phase == Complete {
		cm.Finalizers = slices.DeleteFunc(cm.Finalizers, func(f string) bool { return f == leaseFinalizer })
	} else if !slices.Contains(cm.Finalizers, leaseFinalizer) {
		cm.Finalizers = append(cm.Finalizers, leaseFinalizer)
	}
	if previous == nil {
		return s.Client.Create(ctx, cm)
	}
	return s.Client.Update(ctx, cm)
}

package aws

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

const routeFinalizer = "cloud-provisioning.appmana.com/aws-route"

type ConfigMapJournal struct {
	Client    client.Client
	Namespace string
}

func (j ConfigMapJournal) key(key string) client.ObjectKey {
	hash := sha256.Sum256([]byte(key))
	return client.ObjectKey{Namespace: j.Namespace, Name: fmt.Sprintf("aws-route-%x", hash[:16])}
}

func (j ConfigMapJournal) Load(ctx context.Context, key string) (*RouteRecord, error) {
	if j.Client == nil || j.Namespace == "" || key == "" {
		return nil, fmt.Errorf("route journal client, namespace and key are required")
	}
	cm := &corev1.ConfigMap{}
	if err := j.Client.Get(ctx, j.key(key), cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var record RouteRecord
	if err := json.Unmarshal([]byte(cm.Data["route.json"]), &record); err != nil {
		return nil, err
	}
	if record.Key != key {
		return nil, fmt.Errorf("route journal key mismatch")
	}
	record.Version = string(cm.UID) + "/" + cm.ResourceVersion
	record.Retiring = cm.DeletionTimestamp != nil
	return &record, nil
}

func (j ConfigMapJournal) Save(ctx context.Context, old, next *RouteRecord) error {
	if j.Client == nil || j.Namespace == "" || next == nil || next.Key == "" {
		return fmt.Errorf("route journal client, namespace and next record are required")
	}
	key := j.key(next.Key)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	if old != nil {
		if old.Key != next.Key {
			return fmt.Errorf("route journal key cannot change")
		}
		if err := j.Client.Get(ctx, key, cm); err != nil {
			return err
		}
		if string(cm.UID)+"/"+cm.ResourceVersion != old.Version {
			return fmt.Errorf("route journal changed concurrently")
		}
	}
	if cm.DeletionTimestamp != nil && next.Phase != "Deleting" && next.Phase != "Absent" {
		shrinking := old != nil && old.Target == next.Target && len(next.Leases) < len(old.Leases)
		for lease := range next.Leases {
			if old == nil || !old.Leases[lease] {
				shrinking = false
			}
		}
		if !shrinking {
			return fmt.Errorf("route journal is deleting; release its leases first")
		}
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["route.json"] = string(raw)
	if next.Phase == "Absent" {
		cm.Finalizers = slices.DeleteFunc(cm.Finalizers, func(v string) bool { return v == routeFinalizer })
	} else if !slices.Contains(cm.Finalizers, routeFinalizer) {
		cm.Finalizers = append(cm.Finalizers, routeFinalizer)
	}
	if old == nil {
		return j.Client.Create(ctx, cm)
	}
	return j.Client.Update(ctx, cm)
}

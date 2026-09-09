package attachment

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Consumer identifies a required recipient, not a pod selected at check time.
// These identities must be retained through withdrawal. Remote Document is the
// intended render for the mesh snapshot, not whatever its Secret happens to hold.
type Consumer struct {
	NodeName   string `json:"nodeName"`
	NodeUID    string `json:"nodeUID"`
	Site       bool   `json:"site"`
	SecretName string `json:"secretName,omitempty"`
	SecretUID  string `json:"secretUID,omitempty"`
	Document   []byte `json:"document,omitempty"`
}
type ConsumerVerifier struct{ Reader client.Reader }

// Applied checks all recipients and rechecks the source snapshot last. Missing
// recipients are pending, never evidence of withdrawal. It makes no writes.
func (v ConsumerVerifier) Applied(ctx context.Context, mesh *corev1.Secret, consumers []Consumer) (bool, error) {
	if v.Reader == nil || mesh == nil || mesh.UID == "" || mesh.Namespace == "" || len(consumers) == 0 {
		return false, fmt.Errorf("reader, original mesh snapshot and required consumers needed")
	}
	seen := map[string]bool{}
	for _, c := range consumers {
		if c.NodeName == "" || c.NodeUID == "" || seen[c.NodeName] {
			return false, fmt.Errorf("missing or duplicate consumer identity")
		}
		seen[c.NodeName] = true
		if !c.Site && (c.SecretName == "" || c.SecretUID == "" || len(c.Document) == 0) {
			return false, fmt.Errorf("remote consumer requires intended document and Secret identity")
		}
	}
	current := &corev1.Secret{}
	if err := v.Reader.Get(ctx, client.ObjectKeyFromObject(mesh), current); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if current.UID != mesh.UID || current.DeletionTimestamp != nil {
		return false, nil
	}
	if !samePublicationSource(mesh.Data, current.Data) {
		return false, nil
	}
	expected, err := tunnel.SitePeerHash(mesh.Data)
	if err != nil {
		return false, err
	}
	actual, err := tunnel.SitePeerHash(current.Data)
	if err != nil {
		return false, err
	}
	if actual != expected {
		return false, nil
	}
	nodes := &corev1.NodeList{}
	if err := v.Reader.List(ctx, nodes); err != nil {
		return false, err
	}
	notReady := map[string]bool{}
	for _, node := range nodes.Items {
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		notReady[node.Name] = !ready
	}
	for _, c := range consumers {
		node := &corev1.Node{}
		if err := v.Reader.Get(ctx, client.ObjectKey{Name: c.NodeName}, node); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if string(node.UID) != c.NodeUID || node.DeletionTimestamp != nil {
			return false, nil
		}
		if c.Site {
			if !tunnel.SiteConverged(current.Data, c.NodeName, c.NodeUID, notReady) {
				return false, nil
			}
			continue
		}
		secret := &corev1.Secret{}
		if err := v.Reader.Get(ctx, client.ObjectKey{Namespace: mesh.Namespace, Name: c.SecretName}, secret); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if string(secret.UID) != c.SecretUID || secret.DeletionTimestamp != nil || !bytes.Equal(secret.Data[tunnel.CloudPeersKey], c.Document) || secret.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(c.Document) {
			return false, nil
		}
	}
	// resourceVersion includes API endpoints and source fields not present in the
	// site peer hash. Require the observed source to stay stable across all reads.
	last := &corev1.Secret{}
	if err := v.Reader.Get(ctx, client.ObjectKeyFromObject(mesh), last); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return last.UID == current.UID && last.ResourceVersion == current.ResourceVersion && last.DeletionTimestamp == nil, nil
}

// Receipts may change while consumers apply a snapshot. Every other data field,
// including API backends and gateway intent, must still match that snapshot.
func samePublicationSource(a, b map[string][]byte) bool {
	for key, value := range a {
		if !strings.HasPrefix(key, tunnel.SiteAppliedPrefix) && !bytes.Equal(value, b[key]) {
			return false
		}
	}
	for key, value := range b {
		if !strings.HasPrefix(key, tunnel.SiteAppliedPrefix) && !bytes.Equal(value, a[key]) {
			return false
		}
	}
	return true
}

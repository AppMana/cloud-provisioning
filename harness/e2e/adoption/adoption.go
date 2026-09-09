package adoption

import (
	"encoding/json"
	"fmt"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func DecodeDaemonSet(raw []byte) (*appsv1.DaemonSet, error) {
	var ds appsv1.DaemonSet
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, err
	}
	if ds.Name != "cloud-provisioning-dialer-remote" || ds.Namespace != "cloud-provisioning" || ds.UID == "" || len(ds.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("invalid Linux remote adoption DaemonSet")
	}
	return &ds, nil
}

func ReadyPod(ds *appsv1.DaemonSet, raw []byte, node string) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := json.Unmarshal(raw, &pods); err != nil {
		return nil, err
	}
	var matching []*corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName != node || p.Namespace != ds.Namespace || p.DeletionTimestamp != nil {
			continue
		}
		for _, o := range p.OwnerReferences {
			if o.Kind == "DaemonSet" && o.Name == ds.Name && o.UID == ds.UID && o.Controller != nil && *o.Controller {
				matching = append(matching, p)
				break
			}
		}
	}
	if len(matching) != 1 {
		return nil, fmt.Errorf("expected one current adoption Pod on %s, got %d", node, len(matching))
	}
	p := matching[0]
	if len(p.Spec.Containers) != len(ds.Spec.Template.Spec.Containers) {
		return nil, fmt.Errorf("adoption Pod template mismatch")
	}
	for i, c := range ds.Spec.Template.Spec.Containers {
		if p.Spec.Containers[i].Name != c.Name || p.Spec.Containers[i].Image != c.Image || p.Spec.Containers[i].ImagePullPolicy != c.ImagePullPolicy {
			return nil, fmt.Errorf("adoption Pod image or pull policy differs from desired DaemonSet")
		}
	}
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return nil, fmt.Errorf("adoption Pod %s is not Ready", p.Name)
	}
	if len(p.Status.ContainerStatuses) != len(p.Spec.Containers) {
		return nil, fmt.Errorf("adoption Pod container status incomplete")
	}
	for _, c := range p.Status.ContainerStatuses {
		if !c.Ready || c.State.Running == nil || c.ContainerID == "" || c.ImageID == "" {
			return nil, fmt.Errorf("adoption container %s is not running and Ready", c.Name)
		}
	}
	return p, nil
}

func RequiredImages(site string, remote *appsv1.DaemonSet) []string {
	images := []string{site}
	for _, containers := range [][]corev1.Container{remote.Spec.Template.Spec.InitContainers, remote.Spec.Template.Spec.Containers} {
		for _, c := range containers {
			found := false
			for _, existing := range images {
				if existing == c.Image {
					found = true
					break
				}
			}
			if !found {
				images = append(images, c.Image)
			}
		}
	}
	return images
}

// Receipt records content acknowledgment, not attribution to a particular
// Pod or proof of continuity. Never persist the Secret's payload in row reports.
type Receipt struct {
	SecretUID       string `json:"secretUID"`
	ResourceVersion string `json:"resourceVersion"`
	DesiredSHA256   string `json:"desiredSHA256"`
	AppliedSHA256   string `json:"appliedSHA256"`
}

func AppliedReceipt(raw []byte, machine string) (*Receipt, error) {
	var secret corev1.Secret
	if err := json.Unmarshal(raw, &secret); err != nil {
		return nil, fmt.Errorf("invalid adoption Secret response")
	}
	if machine == "" || secret.Name != tunnel.AdoptionSecretName(machine) || secret.Namespace != "cloud-provisioning" || secret.UID == "" || secret.ResourceVersion == "" || secret.DeletionTimestamp != nil {
		return nil, fmt.Errorf("adoption Secret identity is missing or stale")
	}
	peers := secret.Data[tunnel.CloudPeersKey]
	var doc tunnel.PeerListDoc
	if len(peers) == 0 || json.Unmarshal(peers, &doc) != nil || len(doc.Peers) == 0 {
		return nil, fmt.Errorf("adoption Secret has no usable Linux peer list")
	}
	desired := tunnel.HashPeerList(peers)
	applied := secret.Annotations[tunnel.AppliedListAnnotation]
	if applied != desired {
		return nil, fmt.Errorf("current peer list has not been acknowledged")
	}
	return &Receipt{string(secret.UID), secret.ResourceVersion, desired, applied}, nil
}

package adoption

import (
	"encoding/json"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type adoptionFixture struct {
	SiteImage, Node           string
	Daemonset, Pending, Ready json.RawMessage
}

func loadAdoption(t *testing.T) adoptionFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/native-adoption.json")
	if err != nil {
		t.Fatal(err)
	}
	var f adoptionFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}
func TestNativeImagePullFailureCannotQualifyAdoption(t *testing.T) {
	f := loadAdoption(t)
	ds, err := DecodeDaemonSet(f.Daemonset)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadyPod(ds, f.Pending, f.Node); err == nil {
		t.Fatal("native ImagePullBackOff accepted")
	}
	p, err := ReadyPod(ds, f.Ready, f.Node)
	if err != nil {
		t.Fatal(err)
	}
	var before corev1.PodList
	json.Unmarshal(f.Pending, &before)
	if p.UID != before.Items[0].UID {
		t.Fatal("fixture is not the same Pod's recovery")
	}
	images := RequiredImages(f.SiteImage, ds)
	if len(images) != 2 || images[0] != f.SiteImage || images[1] != ds.Spec.Template.Spec.Containers[0].Image {
		t.Fatalf("remote image omitted: %v", images)
	}
	ds.Spec.Template.Spec.InitContainers = []corev1.Container{{Name: "init", Image: images[1]}}
	if !reflect.DeepEqual(images, RequiredImages(f.SiteImage, ds)) {
		t.Fatal("duplicate image staged")
	}
}
func TestAdoptionRejectsStaleOrUnrelatedReadyPods(t *testing.T) {
	f := loadAdoption(t)
	ds, err := DecodeDaemonSet(f.Daemonset)
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]func(*corev1.PodList){
		"old pull policy": func(p *corev1.PodList) { p.Items[0].Spec.Containers[0].ImagePullPolicy = corev1.PullNever },
		"wrong owner":     func(p *corev1.PodList) { p.Items[0].OwnerReferences[0].UID = "other" },
		"wrong node":      func(p *corev1.PodList) { p.Items[0].Spec.NodeName = "other" },
		"old image":       func(p *corev1.PodList) { p.Items[0].Spec.Containers[0].Image = f.SiteImage },
		"terminating":     func(p *corev1.PodList) { now := metav1.Now(); p.Items[0].DeletionTimestamp = &now },
		"missing runtime": func(p *corev1.PodList) { p.Items[0].Status.ContainerStatuses[0].ContainerID = "" },
		"ambiguous":       func(p *corev1.PodList) { p.Items = append(p.Items, p.Items[0]) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			var pods corev1.PodList
			json.Unmarshal(f.Ready, &pods)
			change(&pods)
			raw, _ := json.Marshal(pods)
			if _, err := ReadyPod(ds, raw, f.Node); err == nil {
				t.Fatal("accepted stale adoption")
			}
		})
	}
}

func TestAppliedAdoptionReceipt(t *testing.T) {
	// Synthetic mutations of the native matching-hash contract. This does not
	// simulate network behavior or claim that a Ready Pod wrote the receipt.
	makeSecret := func() corev1.Secret {
		peers := []byte(`{"peers":[{}]}`)
		return corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("worker"), Namespace: "cloud-provisioning", UID: "uid", ResourceVersion: "1", Annotations: map[string]string{tunnel.AppliedListAnnotation: tunnel.HashPeerList(peers)}}, Data: map[string][]byte{tunnel.CloudPeersKey: peers}}
	}
	cases := map[string]func(*corev1.Secret){
		"valid":                  func(s *corev1.Secret) {},
		"wrong machine":          func(s *corev1.Secret) { s.Name = "other" },
		"wrong namespace":        func(s *corev1.Secret) { s.Namespace = "other" },
		"missing UID":            func(s *corev1.Secret) { s.UID = "" },
		"missing version":        func(s *corev1.Secret) { s.ResourceVersion = "" },
		"deleting":               func(s *corev1.Secret) { now := metav1.Now(); s.DeletionTimestamp = &now },
		"missing payload":        func(s *corev1.Secret) { s.Data = nil },
		"invalid payload":        func(s *corev1.Secret) { s.Data[tunnel.CloudPeersKey] = []byte("invalid") },
		"empty list":             func(s *corev1.Secret) { s.Data[tunnel.CloudPeersKey] = []byte(`{"peers":[]}`) },
		"missing acknowledgment": func(s *corev1.Secret) { s.Annotations = nil },
		"stale acknowledgment":   func(s *corev1.Secret) { s.Data[tunnel.CloudPeersKey] = []byte(`{"peers":[{},{}]}`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := makeSecret()
			mutate(&s)
			raw, _ := json.Marshal(s)
			receipt, err := AppliedReceipt(raw, "worker")
			if name == "valid" {
				if err != nil || receipt.DesiredSHA256 != receipt.AppliedSHA256 {
					t.Fatalf("receipt: %v %v", receipt, err)
				}
			} else if err == nil {
				t.Fatal("invalid receipt accepted")
			}
		})
	}
	if _, err := AppliedReceipt([]byte("invalid"), "worker"); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestObservedAdoptionReceipts(t *testing.T) {
	dir := os.Getenv("CLDT_ADOPTION_OBSERVATIONS")
	if dir == "" {
		t.Skip("set CLDT_ADOPTION_OBSERVATIONS for private native Secret snapshots")
	}
	for _, machine := range []string{"aws-relay1", "remote1", "remote2"} {
		raw, err := os.ReadFile(filepath.Join(dir, machine+".json"))
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := AppliedReceipt(raw, machine)
		if err != nil {
			t.Fatalf("%s: %v", machine, err)
		}
		t.Logf("%s: Secret UID %s, resourceVersion %s, applied SHA256 %s", machine, receipt.SecretUID, receipt.ResourceVersion, receipt.AppliedSHA256)
	}
}

// This fixture preserves the observed failure; no synthetic recovery is claimed.
func TestObservedMicroK8sContainerCreationFailure(t *testing.T) {
	raw, err := os.ReadFile("testdata/microk8s-container-error.json")
	if err != nil {
		t.Fatal(err)
	}
	var f adoptionFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	ds, err := DecodeDaemonSet(f.Daemonset)
	if err != nil {
		t.Fatal(err)
	}
	var pods corev1.PodList
	if err := json.Unmarshal(f.Pending, &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Status.ContainerStatuses[0].State.Waiting.Reason != "CreateContainerError" {
		t.Fatal("fixture lost observed failure")
	}
	if _, err := ReadyPod(ds, f.Pending, f.Node); err == nil {
		t.Fatal("native container creation failure qualified")
	}
}

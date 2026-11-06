package gcp

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestProviderContract(t *testing.T) {
	p := Provider{}
	if got := p.GVK().Kind; got != "GCPMachine" {
		t.Fatalf("GVK().Kind = %q, want GCPMachine", got)
	}
	if got := p.GVK().Version; got != "v1beta1" {
		t.Fatalf("GVK().Version = %q, want v1beta1 (CAPG storage version)", got)
	}
	if got := p.ClusterGVK().Kind; got != "GCPCluster" {
		t.Fatalf("ClusterGVK().Kind = %q, want GCPCluster", got)
	}
	vals, err := p.InfraValues(context.Background(), &unstructured.Unstructured{})
	if err != nil {
		t.Fatalf("InfraValues: %v", err)
	}
	scriptB64, _ := vals["providerIdentityScript"].(string)
	rawScript, err := base64.StdEncoding.DecodeString(scriptB64)
	if err != nil || !strings.Contains(string(rawScript), "gce://") {
		t.Fatalf("unexpected providerIdentityScript: %q (err=%v)", string(rawScript), err)
	}
	psB64, _ := vals["providerIdentityPowerShell"].(string)
	rawPS, err := base64.StdEncoding.DecodeString(psB64)
	if err != nil || !strings.Contains(string(rawPS), "gce://") {
		t.Fatalf("unexpected providerIdentityPowerShell: %q (err=%v)", string(rawPS), err)
	}
}

func TestPrepareInfraMachine(t *testing.T) {
	p := Provider{}
	m := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
		"kind":       "GCPMachine",
		"metadata": map[string]any{
			"name": "gcp-worker-1",
			"labels": map[string]any{
				"cloud-provisioning.appmana.com/arch": "arm64",
			},
		},
		"spec": map[string]any{
			"instanceType": "t2a-standard-2",
			"zone":         "us-central1-b",
		},
	}}
	fd, err := p.PrepareInfraMachine(m)
	if err != nil {
		t.Fatalf("PrepareInfraMachine: %v", err)
	}
	if fd != "us-central1-b" {
		t.Fatalf("failureDomain = %q, want us-central1-b", fd)
	}
	if _, found, _ := unstructured.NestedString(m.Object, "spec", "zone"); found {
		t.Fatal("spec.zone must be stripped from GCPMachine (CAPG schema rejects it)")
	}
	pubIP, found, _ := unstructured.NestedBool(m.Object, "spec", "publicIP")
	if !found || !pubIP {
		t.Fatalf("spec.publicIP = %v (found=%v), want true", pubIP, found)
	}
	imgFamily, _, _ := unstructured.NestedString(m.Object, "spec", "imageFamily")
	if imgFamily != DefaultImageFamilyARM64 {
		t.Fatalf("spec.imageFamily = %q, want %q", imgFamily, DefaultImageFamilyARM64)
	}
	meta, _, _ := unstructured.NestedSlice(m.Object, "spec", "additionalMetadata")
	if len(meta) != 1 {
		t.Fatalf("spec.additionalMetadata len = %d, want 1", len(meta))
	}
	entry, _ := meta[0].(map[string]any)
	if entry["key"] != "startup-script" || !strings.Contains(entry["value"].(string), "cloud-init") {
		t.Fatalf("unexpected startup-script metadata: %+v", entry)
	}
}

func TestValidateCredentialsRef(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(gcpClusterGVK, &unstructured.Unstructured{})
	capiCluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cluster.x-k8s.io/v1beta2",
		"kind":       "Cluster",
		"metadata":   map[string]any{"name": "site-gcp", "namespace": "cloud-provisioning"},
		"spec": map[string]any{
			"infrastructureRef": map[string]any{
				"apiGroup": "infrastructure.cluster.x-k8s.io",
				"kind":     "GCPCluster",
				"name":     "site-gcp",
			},
		},
	}}
	gcpCluster := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
		"kind":       "GCPCluster",
		"metadata":   map[string]any{"name": "site-gcp", "namespace": "cloud-provisioning"},
		"spec": map[string]any{
			"project": "my-proj",
			"region":  "us-central1",
			"credentialsRef": map[string]any{
				"name":      "capg-creds",
				"namespace": "cloud-provisioning",
			},
		},
	}}
	gcpMachine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
		"kind":       "GCPMachine",
		"metadata": map[string]any{
			"name":      "gcp-worker-1",
			"namespace": "cloud-provisioning",
			"labels":    map[string]any{"cluster.x-k8s.io/cluster-name": "site-gcp"},
		},
	}}

	cMissing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(capiCluster, gcpCluster).Build()
	if err := (Provider{}).Validate(context.Background(), cMissing, gcpMachine); err == nil {
		t.Fatal("expected error when credentialsRef Secret is missing")
	}

	cEmptyKey := fake.NewClientBuilder().WithScheme(scheme).WithObjects(capiCluster, gcpCluster, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "capg-creds", Namespace: "cloud-provisioning"},
		Data:       map[string][]byte{"wrong": []byte("{}")},
	}).Build()
	if err := (Provider{}).Validate(context.Background(), cEmptyKey, gcpMachine); err == nil {
		t.Fatal("expected error when credentialsRef Secret lacks \"credentials\" key")
	}

	cValid := fake.NewClientBuilder().WithScheme(scheme).WithObjects(capiCluster, gcpCluster, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "capg-creds", Namespace: "cloud-provisioning"},
		Data:       map[string][]byte{"credentials": []byte(`{"type":"service_account"}`)},
	}).Build()
	if err := (Provider{}).Validate(context.Background(), cValid, gcpMachine); err != nil {
		t.Fatalf("unexpected error with valid credentials Secret: %v", err)
	}
}

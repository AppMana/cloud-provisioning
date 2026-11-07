package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	joingcp "github.com/appmana/cloud-provisioning/controller/pkg/join/gcp"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func applyPatchOps(t *testing.T, orig map[string]any, patchBytes []byte) map[string]any {
	t.Helper()
	rawOrig, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(rawOrig, &out); err != nil {
		t.Fatal(err)
	}
	if len(patchBytes) == 0 {
		return out
	}
	var ops []PatchOp
	if err := json.Unmarshal(patchBytes, &ops); err != nil {
		t.Fatalf("unmarshaling patch ops: %v", err)
	}
	for _, op := range ops {
		parts := splitPointer(op.Path)
		if len(parts) == 0 {
			continue
		}
		cur := out
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[p] = next
			}
			cur = next
		}
		last := parts[len(parts)-1]
		switch op.Op {
		case "add", "replace":
			cur[last] = op.Value
		case "remove":
			delete(cur, last)
		}
	}
	return out
}

func splitPointer(path string) []string {
	if path == "" || path == "/" {
		return nil
	}
	raw := bytes.Split([]byte(path[1:]), []byte("/"))
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		s := string(r)
		s = string(bytes.ReplaceAll([]byte(s), []byte("~1"), []byte("/")))
		s = string(bytes.ReplaceAll([]byte(s), []byte("~0"), []byte("~")))
		out = append(out, s)
	}
	return out
}

func TestMutator_AWSMachineTemplatePatchesSecurityGroupAMIAndPublicIP(t *testing.T) {
	mut := &Mutator{
		ManagedNamespace: "cloud-provisioning",
		AWSProvider: joinaws.Provider{
			SecurityGroupIDs:  []string{"sg-onprem-wireguard"},
			SecurityGroupName: "cloud-provisioning-tunnel",
		},
	}

	rawObj := map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "AWSMachineTemplate",
		"metadata": map[string]any{
			"name":      "aws-m6i-xlarge",
			"namespace": "cloud-provisioning",
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"instanceType": "m6i.xlarge",
				},
			},
		},
	}
	rawBytes, _ := json.Marshal(rawObj)

	review := admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID:       "req-1",
			Kind:      metav1.GroupVersionKind{Group: infraGroup, Version: "v1beta2", Kind: "AWSMachineTemplate"},
			Namespace: "cloud-provisioning",
			Object:    runtime.RawExtension{Raw: rawBytes},
		},
	}
	reqBody, _ := json.Marshal(review)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mutate", bytes.NewReader(reqBody))
	mut.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var outReview admissionv1.AdmissionReview
	if err := json.Unmarshal(rec.Body.Bytes(), &outReview); err != nil {
		t.Fatal(err)
	}
	if outReview.Response == nil || !outReview.Response.Allowed {
		t.Fatalf("expected allowed response, got %+v", outReview.Response)
	}

	patched := applyPatchOps(t, rawObj, outReview.Response.Patch)
	spec, _, _ := unstructured.NestedMap(patched, "spec", "template", "spec")

	if pub, _ := spec["publicIP"].(bool); !pub {
		t.Fatalf("publicIP = %v, want true", spec["publicIP"])
	}
	if prof, _ := spec["iamInstanceProfile"].(string); prof != joinaws.DefaultIAMInstanceProfile {
		t.Fatalf("iamInstanceProfile = %q, want %q", prof, joinaws.DefaultIAMInstanceProfile)
	}
	if org, _ := spec["imageLookupOrg"].(string); org != joinaws.DefaultImageLookupOrg {
		t.Fatalf("imageLookupOrg = %q, want %q", org, joinaws.DefaultImageLookupOrg)
	}
	if baseOS, _ := spec["imageLookupBaseOS"].(string); baseOS != joinaws.DefaultImageLookupBaseOS {
		t.Fatalf("imageLookupBaseOS = %q, want %q", baseOS, joinaws.DefaultImageLookupBaseOS)
	}
	if fmtStr, _ := spec["imageLookupFormat"].(string); fmtStr != joinaws.DefaultImageLookupFormatAMD64 {
		t.Fatalf("imageLookupFormat = %q, want %q", fmtStr, joinaws.DefaultImageLookupFormatAMD64)
	}
	sgs, _ := spec["additionalSecurityGroups"].([]any)
	if len(sgs) != 2 {
		t.Fatalf("additionalSecurityGroups = %+v, want 2 entries (ID + filter)", sgs)
	}
}

func TestMutator_GCPAndAzureTemplates(t *testing.T) {
	mut := &Mutator{
		ManagedNamespace: "cloud-provisioning",
		GCPNetworkTag:    "wireguard-tunnel",
	}

	gcpObj := map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
		"kind":       "GCPMachineTemplate",
		"metadata":   map[string]any{"name": "gcp-n2", "namespace": "cloud-provisioning"},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{"instanceType": "t2a-standard-4"},
			},
		},
	}
	gcpRaw, _ := json.Marshal(gcpObj)
	resp := mut.Admit(context.Background(), &admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: infraGroup, Version: "v1beta1", Kind: "GCPMachineTemplate"},
		Namespace: "cloud-provisioning",
		Object:    runtime.RawExtension{Raw: gcpRaw},
	})
	patchedGCP := applyPatchOps(t, gcpObj, resp.Patch)
	gcpSpec, _, _ := unstructured.NestedMap(patchedGCP, "spec", "template", "spec")
	if pub, _ := gcpSpec["publicIP"].(bool); !pub {
		t.Fatal("GCPMachineTemplate publicIP must default to true")
	}
	if fam, _ := gcpSpec["imageFamily"].(string); fam != joingcp.DefaultImageFamilyARM64 {
		t.Fatalf("GCPMachineTemplate imageFamily = %q, want %q", fam, joingcp.DefaultImageFamilyARM64)
	}
	tags, _ := gcpSpec["additionalNetworkTags"].([]any)
	if len(tags) != 1 || tags[0] != "wireguard-tunnel" {
		t.Fatalf("GCPMachineTemplate additionalNetworkTags = %v, want [wireguard-tunnel]", tags)
	}

	azureObj := map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta1",
		"kind":       "AzureMachineTemplate",
		"metadata":   map[string]any{"name": "az-d4s", "namespace": "cloud-provisioning"},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{"vmSize": "Standard_D4s_v5"},
			},
		},
	}
	azRaw, _ := json.Marshal(azureObj)
	azResp := mut.Admit(context.Background(), &admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: infraGroup, Version: "v1beta1", Kind: "AzureMachineTemplate"},
		Namespace: "cloud-provisioning",
		Object:    runtime.RawExtension{Raw: azRaw},
	})
	patchedAz := applyPatchOps(t, azureObj, azResp.Patch)
	azSpec, _, _ := unstructured.NestedMap(patchedAz, "spec", "template", "spec")
	if pub, _ := azSpec["allocatePublicIP"].(bool); !pub {
		t.Fatal("AzureMachineTemplate allocatePublicIP must default to true")
	}
}

func TestMutator_ClustersAndNodeGroupClaims(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	clusterGVK := schema.GroupVersionKind{Group: capiGroup, Version: "v1beta2", Kind: "Cluster"}
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: capiGroup, Version: "v1beta2", Kind: "ClusterList"}, &unstructured.UnstructuredList{})

	existingCluster := &unstructured.Unstructured{}
	existingCluster.SetGroupVersionKind(clusterGVK)
	existingCluster.SetNamespace("cloud-provisioning")
	existingCluster.SetName("site-aws")
	_ = unstructured.SetNestedField(existingCluster.Object, "AWSCluster", "spec", "infrastructureRef", "kind")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existingCluster).Build()

	mut := &Mutator{
		Reader:           cl,
		ManagedNamespace: "cloud-provisioning",
		AWSIdentityName:  "aws-static-creds",
		APIVIP:           "10.96.0.1",
		APIServerPort:    "6443",
	}

	awsClusterObj := map[string]any{
		"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
		"kind":       "AWSCluster",
		"metadata":   map[string]any{"name": "site-aws", "namespace": "cloud-provisioning"},
		"spec":       map[string]any{"region": "us-west-2"},
	}
	awsClusterRaw, _ := json.Marshal(awsClusterObj)
	clusterResp := mut.Admit(context.Background(), &admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: infraGroup, Version: "v1beta2", Kind: "AWSCluster"},
		Namespace: "cloud-provisioning",
		Object:    runtime.RawExtension{Raw: awsClusterRaw},
	})
	patchedCluster := applyPatchOps(t, awsClusterObj, clusterResp.Patch)
	anns, _, _ := unstructured.NestedStringMap(patchedCluster, "metadata", "annotations")
	if anns[managedByAnnotation] != "external" {
		t.Fatalf("AWSCluster annotations = %v, want managed-by=external", anns)
	}
	host, _, _ := unstructured.NestedString(patchedCluster, "spec", "controlPlaneEndpoint", "host")
	if host != "10.96.0.1" {
		t.Fatalf("AWSCluster controlPlaneEndpoint.host = %q, want 10.96.0.1", host)
	}
	idName, _, _ := unstructured.NestedString(patchedCluster, "spec", "identityRef", "name")
	if idName != "aws-static-creds" {
		t.Fatalf("AWSCluster identityRef.name = %q, want aws-static-creds", idName)
	}

	pngcObj := map[string]any{
		"apiVersion": "cloud-provisioning.appmana.com/v1alpha1",
		"kind":       "ProvisionedNodeGroupClaim",
		"metadata":   map[string]any{"name": "aws-burst-pool", "namespace": "cloud-provisioning"},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"infrastructureRef": map[string]any{
						"kind": "AWSMachine",
						"name": "aws-m6i-xlarge",
					},
				},
			},
		},
	}
	pngcRaw, _ := json.Marshal(pngcObj)
	pngcResp := mut.Admit(context.Background(), &admissionv1.AdmissionRequest{
		Kind:      metav1.GroupVersionKind{Group: cloudProvisioningGroup, Version: "v1alpha1", Kind: "ProvisionedNodeGroupClaim"},
		Namespace: "cloud-provisioning",
		Object:    runtime.RawExtension{Raw: pngcRaw},
	})
	patchedPNGC := applyPatchOps(t, pngcObj, pngcResp.Patch)
	if reps, _, _ := unstructured.NestedFloat64(patchedPNGC, "spec", "replicas"); reps != 1 {
		t.Fatalf("ProvisionedNodeGroupClaim spec.replicas = %v, want 1", reps)
	}
	if cn, _, _ := unstructured.NestedString(patchedPNGC, "spec", "template", "spec", "clusterName"); cn != "site-aws" {
		t.Fatalf("ProvisionedNodeGroupClaim template clusterName = %q, want site-aws", cn)
	}
	if kind, _, _ := unstructured.NestedString(patchedPNGC, "spec", "template", "spec", "infrastructureRef", "kind"); kind != "AWSMachineTemplate" {
		t.Fatalf("ProvisionedNodeGroupClaim infrastructureRef.kind = %q, want AWSMachineTemplate", kind)
	}
	if grp, _, _ := unstructured.NestedString(patchedPNGC, "spec", "template", "spec", "infrastructureRef", "apiGroup"); grp != infraGroup {
		t.Fatalf("ProvisionedNodeGroupClaim infrastructureRef.apiGroup = %q, want %q", grp, infraGroup)
	}
	if _, err := (&corev1.Secret{}).Marshal(); err != nil {
		t.Fatal(err)
	}
}

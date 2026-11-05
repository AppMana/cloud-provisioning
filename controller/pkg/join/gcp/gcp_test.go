package gcp

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestProviderContract(t *testing.T) {
	p := Provider{}
	if got := p.GVK().Kind; got != "GCPMachine" {
		t.Fatalf("GVK().Kind = %q, want GCPMachine", got)
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

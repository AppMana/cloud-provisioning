package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
)

func TestImportedControlPlaneDeclaresExternalManagementForCAPIDeletion(t *testing.T) {
	// CAPI 1.11.1 skipped the live Windows pre-drain hook because the site has
	// no CAPI-owned control-plane Machines and this status field was absent.
	k := &fakeKube{objects: map[string]string{"config": `apiVersion: v1
kind: Config
current-context: site
contexts:
- name: site
  context: {cluster: site, user: admin}
clusters:
- name: site
  cluster: {server: "https://10.10.0.10:6443", certificate-authority-data: Y2E=}
users:
- name: admin
  user: {token: unit-fixture-token}
`}}
	c := &Controller{Kube: k.client()}
	if err := c.ImportControlPlane(context.Background(), "cloud-provisioning", "site"); err != nil {
		t.Fatal(err)
	}
	for _, call := range k.calls {
		if !contains(call, "patch") || !contains(call, "importedcontrolplane") {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(call[len(call)-1]), &body); err != nil {
			t.Fatal(err)
		}
		managed, found, err := unstructured.NestedBool(body, "status", "externalManagedControlPlane")
		if err != nil || !found || !managed {
			t.Fatal("import does not describe the control plane as externally managed")
		}
		return
	}
	t.Fatal("imported control-plane status was not published")
}

type associationReader struct {
	fakeNode
	refName    string
	providerID string
}

func (n *associationReader) Exec(_ context.Context, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	id := n.providerID
	if id == "" {
		id = "containernet://remote1"
	}
	switch {
	case strings.Contains(command, "/readyz"):
		return []byte("ok"), nil
	case strings.Contains(command, "get machine remote1"):
		return []byte(fmt.Sprintf(`{"spec":{"providerID":%q},"status":{"phase":"Running","nodeRef":{"name":%q}}}`, id, n.refName)), nil
	case strings.Contains(command, "get node remote1"):
		return []byte(fmt.Sprintf(`{"metadata":{"uid":"new-node"},"spec":{"providerID":%q}}`, id)), nil
	default:
		return nil, fmt.Errorf("unexpected operation: %s", command)
	}
}

// The fresh kube-router run had Ready Nodes and Provisioned Machines, but no
// CAPI association because its imported cluster was not connected/initialized.
func TestReadyNodeDoesNotSubstituteForCAPIAssociation(t *testing.T) {
	for _, name := range []string{"", "different-node", "remote1"} {
		t.Run("name="+name, func(t *testing.T) {
			reader := &associationReader{refName: name}
			c := &Controller{Kube: &kube.Client{Bastion: reader, ControlPlanes: []string{"10.10.0.10"}}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := c.WaitAssociation(ctx, "cloud-provisioning", "remote1", "remote1")
			if (err == nil) != (name == "remote1") {
				t.Fatalf("nodeRef name %q: %v", name, err)
			}
		})
	}
}

func TestImportedConnectionRetainsCredentialsAndUsesClusterService(t *testing.T) {
	raw := []byte(`apiVersion: v1
kind: Config
current-context: site
contexts:
- name: site
  context: {cluster: site, user: admin}
clusters:
- name: site
  cluster: {server: "https://127.0.0.1:6443", certificate-authority-data: Y2E=}
users:
- name: admin
  user: {token: unit-fixture-token}
`)
	got, err := selfHostedConnection(raw)
	if err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.Load(got)
	if err != nil {
		t.Fatal(err)
	}
	if config.Clusters["site"].Server != "https://kubernetes.default.svc:443" || string(config.Clusters["site"].CertificateAuthorityData) != "ca" || config.AuthInfos["admin"].Token != "unit-fixture-token" {
		t.Fatal("import changed authentication or failed to replace the node-local API address")
	}
	if _, err := selfHostedConnection([]byte(`{"kind":"Config"}`)); err == nil {
		t.Fatal("accepted missing connection context")
	}
}

// CAPA's observed provider ID must pass the same identity gate as a local VM.
func TestCAPAAssociationUsesObservedProviderIdentity(t *testing.T) {
	reader := &associationReader{refName: "remote1", providerID: "aws:///us-west-2a/i-0c1487c421cdf67e8"}
	c := &kube.Client{Bastion: reader, ControlPlanes: []string{"10.10.0.10"}}
	if err := c.WaitMachineAssociation(context.Background(), "cloud-provisioning", "remote1", "remote1"); err != nil {
		t.Fatal(err)
	}
}

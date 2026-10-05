package acceptance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func writeKubeconfig(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	doc := `apiVersion: v1
kind: Config
clusters:
- name: target
  cluster:
    server: ` + server + `
    insecure-skip-tls-verify: true
contexts:
- name: target
  context: {cluster: target, user: reader}
- name: other
  context: {cluster: target, user: reader}
current-context: target
users:
- name: reader
  user: {token: not-a-real-token}
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Only the named file decides the cluster. $KUBECONFIG pointing
// somewhere else, as it does on a workstation whose default context is
// production, changes nothing.
func TestConfigReadsOnlyTheNamedKubeconfig(t *testing.T) {
	path := writeKubeconfig(t, "https://198.51.100.7:6443")
	t.Setenv("KUBECONFIG", writeKubeconfig(t, "https://203.0.113.9:6443"))
	config, err := Config(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "https://198.51.100.7:6443" {
		t.Errorf("host = %s, want the named file's server", config.Host)
	}
	if _, err := Config("", ""); err == nil {
		t.Error("an empty kubeconfig path fell back to some default")
	}
	if _, err := Config(path, "production"); err == nil {
		t.Error("a context the file does not have was accepted")
	}
}

// The precondition client cannot write: every mutating verb is refused
// before it leaves the process, and reads still work.
func TestTheReadOnlyClientRefusesEveryWrite(t *testing.T) {
	var methods []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"NamespaceList","apiVersion":"v1","items":[]}`))
	}))
	defer server.Close()
	config, err := Config(writeKubeconfig(t, server.URL), "")
	if err != nil {
		t.Fatal(err)
	}
	clientset, err := kubernetes.NewForConfig(ReadOnly(config))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatalf("a read was refused: %v", err)
	}
	writes := []error{
		func() error {
			_, err := clientset.CoreV1().Namespaces().Create(ctx, nil, metav1.CreateOptions{})
			return err
		}(),
		clientset.CoreV1().Namespaces().Delete(ctx, "kube-system", metav1.DeleteOptions{}),
		func() error {
			_, err := clientset.CoreV1().Nodes().Patch(ctx, "n", "application/merge-patch+json", []byte(`{}`), metav1.PatchOptions{})
			return err
		}(),
	}
	for i, err := range writes {
		var refused ErrWrite
		if !errors.As(err, &refused) {
			t.Errorf("write %d was not refused locally: %v", i, err)
		}
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("the server saw a %s", m)
		}
	}
}

package k0s

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"regexp"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/clientcmd"
)

func TestRandomToken(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9]+$`)
	for _, n := range []int{6, 16} {
		tok, err := randomToken(n)
		if err != nil {
			t.Fatalf("randomToken(%d): %v", n, err)
		}
		if len(tok) != n {
			t.Errorf("randomToken(%d) = %q, want length %d", n, tok, n)
		}
		if !valid.MatchString(tok) {
			t.Errorf("randomToken(%d) = %q, want only [a-z0-9] per the kubeadm/k0s bootstrap-token charset", n, tok)
		}
	}
}

func TestGzipBase64_MatchesK0sOwnDecodePath(t *testing.T) {
	// k0s decodes a join token via: base64-decode, then gzip-decompress
	// (pkg/token/joindecode.go, mirrored here) -- confirmed by reading
	// k0s's own source. This test proves gzipBase64's output survives
	// exactly that round trip, not just "gzip round-trips with itself".
	const payload = "hello from a bootstrap kubeconfig"
	encoded, err := gzipBase64([]byte(payload))
	if err != nil {
		t.Fatalf("gzipBase64: %v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer gz.Close()
	decoded, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if string(decoded) != payload {
		t.Errorf("round trip = %q, want %q", decoded, payload)
	}
}

func TestBuildKubeconfig_IsValidKubeconfigWithExpectedFields(t *testing.T) {
	kubeconfig, err := buildKubeconfig("https://10.101.0.1:6443", []byte("fake-ca-cert"), "abc123.def456ghi789jklm")
	if err != nil {
		t.Fatalf("buildKubeconfig: %v", err)
	}

	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		t.Fatalf("resulting bytes aren't a parseable kubeconfig: %v", err)
	}
	cluster, ok := cfg.Clusters["k0s"]
	if !ok {
		t.Fatal("kubeconfig has no \"k0s\" cluster entry")
	}
	if cluster.Server != "https://10.101.0.1:6443" {
		t.Errorf("cluster.Server = %q, want the cluster's API address", cluster.Server)
	}
	if string(cluster.CertificateAuthorityData) != "fake-ca-cert" {
		t.Errorf("cluster.CertificateAuthorityData = %q, want the supplied CA cert", cluster.CertificateAuthorityData)
	}
	auth, ok := cfg.AuthInfos["kubelet-bootstrap"]
	if !ok {
		t.Fatal("kubeconfig has no \"kubelet-bootstrap\" auth info -- this is the exact user name k0s itself expects (pkg/token/kubeconfig.go WorkerTokenAuthName)")
	}
	if auth.Token != "abc123.def456ghi789jklm" {
		t.Errorf("auth.Token = %q, want the bootstrap token string", auth.Token)
	}
}

func TestJoinValues_EndToEnd(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: metav1.NamespaceSystem},
			Data:       map[string]string{"ca.crt": "fake-ca-cert"},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "jarvis"},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.36.2+k0s"}},
		},
	)
	p := &Provider{Client: client, APIAddress: "https://10.101.0.1:6443", TTL: 2 * time.Hour}

	values, err := p.JoinValues(context.Background())
	if err != nil {
		t.Fatalf("JoinValues: %v", err)
	}

	if values["k0sVersion"] != "v1.36.2+k0s" {
		t.Errorf("k0sVersion = %v, want it introspected from an existing Node, not hardcoded", values["k0sVersion"])
	}
	joinToken, _ := values["joinToken"].(string)
	if joinToken == "" {
		t.Fatal("joinToken is empty")
	}

	// Prove the token is genuinely usable: decode it exactly as k0s
	// would, and confirm a bootstrap-token Secret matching its embedded
	// token was actually created in kube-system (not just constructed
	// client-side and thrown away).
	raw, err := base64.StdEncoding.DecodeString(joinToken)
	if err != nil {
		t.Fatalf("joinToken isn't valid base64: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("joinToken isn't valid gzip: %v", err)
	}
	kubeconfigBytes, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("reading decompressed joinToken: %v", err)
	}
	cfg, err := clientcmd.Load(kubeconfigBytes)
	if err != nil {
		t.Fatalf("decoded joinToken isn't a valid kubeconfig: %v", err)
	}
	token := cfg.AuthInfos["kubelet-bootstrap"].Token
	tokenID := token[:6]

	secret, err := client.CoreV1().Secrets(metav1.NamespaceSystem).Get(context.Background(), "bootstrap-token-"+tokenID, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected a bootstrap-token-%s secret to exist in kube-system: %v", tokenID, err)
	}
	if secret.Type != "bootstrap.kubernetes.io/token" {
		t.Errorf("secret type = %q, want bootstrap.kubernetes.io/token", secret.Type)
	}
	if secret.StringData["usage-bootstrap-authentication"] != "true" {
		t.Errorf("usage-bootstrap-authentication = %q, want \"true\" -- without it the API server won't accept this as an authentication token", secret.StringData["usage-bootstrap-authentication"])
	}
}

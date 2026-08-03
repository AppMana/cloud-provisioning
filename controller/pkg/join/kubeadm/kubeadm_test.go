package kubeadm

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func selfSignedCA(t *testing.T) (pemBytes []byte, spkiHash string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kubernetes"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing cert: %v", err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "sha256:" + hex.EncodeToString(sum[:])
}

func TestJoinValues_MintsKubeadmShapedCredentials(t *testing.T) {
	caPEM, wantHash := selfSignedCA(t)
	clientset := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kube-system"},
			Data:       map[string]string{"ca.crt": string(caPEM)},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "cp"},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.1"}},
		},
	)
	p := &Provider{Client: clientset, APIAddress: "https://10.2.0.19:6443", TTL: 2 * time.Hour}

	values, err := p.JoinValues(context.Background())
	if err != nil {
		t.Fatalf("JoinValues: %v", err)
	}

	token := values["joinToken"].(string)
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(parts[0]) != 6 || len(parts[1]) != 16 {
		t.Errorf("joinToken %q is not kubeadm's <6>.<16> format", token)
	}
	if values["joinEndpoint"] != "10.2.0.19:6443" {
		t.Errorf("joinEndpoint = %v, want bare host:port", values["joinEndpoint"])
	}
	if values["caCertHash"] != wantHash {
		t.Errorf("caCertHash = %v, want the SPKI sha256 pin %s (kubeadm hashes the Subject Public Key Info, not the whole cert)", values["caCertHash"], wantHash)
	}
	if values["kubernetesVersion"] != "v1.33.1" {
		t.Errorf("kubernetesVersion = %v, want the live node's", values["kubernetesVersion"])
	}

	// The minted Secret must carry BOTH usages: signing is what lets
	// kubeadm's cluster-info discovery validate the JWS, and it's the
	// one k0s's flow doesn't need -- an easy one to drop by copy-paste.
	secret, err := clientset.CoreV1().Secrets("kube-system").Get(context.Background(), "bootstrap-token-"+parts[0], metav1.GetOptions{})
	if err != nil {
		t.Fatalf("bootstrap token secret not created: %v", err)
	}
	if secret.StringData["usage-bootstrap-signing"] != "true" {
		t.Error("usage-bootstrap-signing missing -- kubeadm discovery cannot validate cluster-info without it")
	}
	if secret.StringData["usage-bootstrap-authentication"] != "true" {
		t.Error("usage-bootstrap-authentication missing")
	}
}

func TestHostPort_DefaultsPort(t *testing.T) {
	got, err := hostPort("https://10.2.0.19")
	if err != nil || got != "10.2.0.19:6443" {
		t.Errorf("hostPort = (%q, %v), want 10.2.0.19:6443", got, err)
	}
	if _, err := hostPort("not a url"); err == nil {
		t.Error("expected an error for a non-URL APIAddress")
	}
}

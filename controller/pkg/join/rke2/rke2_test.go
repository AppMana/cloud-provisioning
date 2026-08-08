package rke2

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func selfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "rke2-server-ca@1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// What differs from k3s, and only that: the supervisor port, the
// version key and suffix, and the bootstrap group. The format and
// hash are pkg/join/k3s's, tested there.
func TestJoinValues_CarriesRKE2sOwnConstants(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: metav1.NamespaceSystem},
			Data:       map[string]string{"ca.crt": string(selfSignedCAPEM(t))},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "cp"},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.4+rke2r1"}},
		},
	)
	p := &Provider{Client: client, APIAddress: "https://10.101.0.1:6443", TTL: 10 * time.Minute}

	values, err := p.JoinValues(context.Background())
	if err != nil {
		t.Fatalf("JoinValues: %v", err)
	}
	if v := values["joinServerURL"]; v != "https://10.101.0.1:9345" {
		t.Errorf("joinServerURL %v does not point at RKE2's supervisor port", v)
	}
	if v := values["rke2Version"]; v != "v1.33.4+rke2r1" {
		t.Errorf("rke2Version %v is not the kubelet's own version", v)
	}
	if v := values["apiEndpoint"]; v != "10.101.0.1:6443" {
		t.Errorf("apiEndpoint %v is not the API's own host:port", v)
	}

	token, _ := values["joinToken"].(string)
	m := regexp.MustCompile(`^K10[0-9a-f]{64}::([a-z0-9]{6})\.[a-z0-9]{16}$`).FindStringSubmatch(token)
	if m == nil {
		t.Fatalf("joinToken %q is not in the secure format", token)
	}
	secret, err := client.CoreV1().Secrets(metav1.NamespaceSystem).Get(context.Background(), "bootstrap-token-"+m[1], metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the minted secret does not exist: %v", err)
	}
	if g := secret.StringData["auth-extra-groups"]; g != "system:bootstrappers:rke2:default-node-token" {
		t.Errorf("auth-extra-groups %q is not rke2's node-token group", g)
	}
}

// A k3s cluster's version must not produce an RKE2 install pin.
func TestAK3sClusterRefusesAnRKE2Token(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: metav1.NamespaceSystem},
			Data:       map[string]string{"ca.crt": string(selfSignedCAPEM(t))},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "cp"},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.33.3+k3s1"}},
		},
	)
	p := &Provider{Client: client, APIAddress: "https://10.101.0.1:6443", TTL: 10 * time.Minute}
	if _, err := p.JoinValues(context.Background()); err == nil || !strings.Contains(err.Error(), "+rke2") {
		t.Fatalf("a k3s kubelet version did not refuse an rke2 join: %v", err)
	}
}

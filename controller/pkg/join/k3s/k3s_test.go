package k3s

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
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// selfSignedCAPEM returns a self-signed CA certificate as PEM bytes,
// plus the parsed certificate, for building bundles.
func selfSignedCAPEM(t *testing.T, cn string) ([]byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
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
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert, key
}

func clusterWith(caPEM []byte, kubeletVersion string) *fake.Clientset {
	return fake.NewSimpleClientset(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: metav1.NamespaceSystem},
			Data:       map[string]string{"ca.crt": string(caPEM)},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "cp"},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion: kubeletVersion,
			}},
		},
	)
}

// The whole point of the provider: a token k3s's own clientaccess
// parser accepts, minted entirely through the API. Format verified
// against k3s pkg/clientaccess/token.go: "K10" + hex sha256 of the CA
// bundle + "::" + a kubeadm bootstrap token string (<id>.<secret>),
// which parseToken routes to NewBootstrapTokenString, and the
// supervisor's HasRole admits via the system:bootstrappers group the
// apiserver's bootstrap-token authenticator attaches
// (enable-bootstrap-token-auth is set unconditionally,
// pkg/daemons/control/server.go).
func TestJoinValues_MintsK3sShapedCredentials(t *testing.T) {
	caPEM, _, _ := selfSignedCAPEM(t, "k3s-server-ca@1")
	client := clusterWith(caPEM, "v1.33.3+k3s1")
	p := &Provider{Client: client, APIAddress: "https://10.101.0.1:6443", TTL: 10 * time.Minute}

	values, err := p.JoinValues(context.Background())
	if err != nil {
		t.Fatalf("JoinValues: %v", err)
	}

	token, _ := values["joinToken"].(string)
	re := regexp.MustCompile(`^K10([0-9a-f]{64})::([a-z0-9]{6})\.([a-z0-9]{16})$`)
	m := re.FindStringSubmatch(token)
	if m == nil {
		t.Fatalf("joinToken %q is not K10<sha256>::<id>.<secret>", token)
	}

	// Single self-signed cert: k3s hashCA hashes the literal bytes of
	// the bundle, exactly as served by /cacerts and published through
	// --root-ca-file into kube-root-ca.crt.
	sum := sha256.Sum256(caPEM)
	if m[1] != hex.EncodeToString(sum[:]) {
		t.Errorf("CA hash %s is not the sha256 of the bundle's literal bytes", m[1])
	}

	secret, err := client.CoreV1().Secrets(metav1.NamespaceSystem).Get(context.Background(), "bootstrap-token-"+m[2], metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the minted secret does not exist: %v", err)
	}
	if secret.Type != corev1.SecretType("bootstrap.kubernetes.io/token") {
		t.Errorf("secret type %q is not a bootstrap token", secret.Type)
	}
	// Mirror `k3s token create`'s own defaults (pkg/kubeadm/token.go):
	// both usages, and the k3s node-token group.
	for _, key := range []string{"usage-bootstrap-authentication", "usage-bootstrap-signing"} {
		if secret.StringData[key] != "true" {
			t.Errorf("secret lacks %s=true", key)
		}
	}
	if g := secret.StringData["auth-extra-groups"]; g != "system:bootstrappers:k3s:default-node-token" {
		t.Errorf("auth-extra-groups %q is not k3s's node-token group", g)
	}
	if secret.StringData["token-secret"] != m[3] {
		t.Errorf("secret's token-secret does not match the minted token")
	}

	if v := values["k3sVersion"]; v != "v1.33.3+k3s1" {
		t.Errorf("k3sVersion %v is not the kubelet's own version", v)
	}
	if v := values["joinServerURL"]; v != "https://10.101.0.1:6443" {
		t.Errorf("joinServerURL %v is not the API address", v)
	}
	if v := values["apiEndpoint"]; v != "10.101.0.1:6443" {
		t.Errorf("apiEndpoint %v is not host:port", v)
	}
}

// k3s hashCA (pkg/clientaccess/token.go) hashes the literal file bytes
// when the bundle holds one cert, so even a trailing newline changes
// the digest. The provider must reproduce that exactly, because the
// agent hashes the bytes it downloads from /cacerts, which are the
// same bytes kube-root-ca.crt carries.
func TestHashCA_SingleCertHashesLiteralBytes(t *testing.T) {
	caPEM, _, _ := selfSignedCAPEM(t, "ca")
	withNewline := append(append([]byte{}, caPEM...), '\n')

	h1, err := hashCA(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := hashCA(withNewline)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Error("a trailing newline did not change the hash, so this is not hashing literal bytes")
	}
	sum := sha256.Sum256(caPEM)
	if h1 != hex.EncodeToString(sum[:]) {
		t.Errorf("hash %s is not the sha256 of the literal bytes", h1)
	}
}

// With more than one cert in the bundle, k3s hashes the DER of the
// root that signs the first cert, so CA renewal keeps old tokens valid
// as long as the root survives. Same algorithm, reproduced.
func TestHashCA_BundleHashesRootDER(t *testing.T) {
	rootPEM, rootCert, rootKey := selfSignedCAPEM(t, "root")

	interKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	interTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "intermediate"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	interDER, err := x509.CreateCertificate(rand.Reader, interTmpl, rootCert, &interKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	interPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: interDER})

	bundle := append(append([]byte{}, interPEM...), rootPEM...)
	h, err := hashCA(bundle)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(rootCert.Raw)
	if h != hex.EncodeToString(sum[:]) {
		t.Errorf("bundle hash %s is not the sha256 of the root's DER", h)
	}
}

// A kubelet version without the +k3s suffix is not a k3s cluster, and
// passing it to get.k3s.io as INSTALL_K3S_VERSION would install
// something other than what the cluster runs. Unlike k0s, whose
// kubelet reports only a prefix of the release tag, a k3s kubelet
// reports the full install version, so nothing needs resolving; but a
// version that is not k3s's at all must refuse loudly.
func TestKubeletVersionMustCarryTheK3sSuffix(t *testing.T) {
	caPEM, _, _ := selfSignedCAPEM(t, "ca")
	client := clusterWith(caPEM, "v1.33.3")
	p := &Provider{Client: client, APIAddress: "https://10.101.0.1:6443", TTL: 10 * time.Minute}
	if _, err := p.JoinValues(context.Background()); err == nil || !strings.Contains(err.Error(), "k3s") {
		t.Fatalf("a non-k3s kubelet version did not refuse: %v", err)
	}
}

func TestHostPort_DefaultsPort(t *testing.T) {
	got, err := hostPort("https://10.0.0.1")
	if err != nil || got != "10.0.0.1:6443" {
		t.Fatalf("hostPort: %q, %v", got, err)
	}
}

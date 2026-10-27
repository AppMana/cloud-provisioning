package microk8s

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var agentNow = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// snapData lays out what a MicroK8s control plane has under
// /var/snap/microk8s: the current symlink, and in the revision's data
// directory the cluster agent's token file, server certificate and args.
func snapData(t *testing.T, root, revision, tokens string) (check string) {
	t.Helper()
	data := filepath.Join(root, revision)
	for _, dir := range []string{"credentials", "certs", "args"} {
		if err := os.MkdirAll(filepath.Join(data, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: agentNow.Add(-time.Hour), NotAfter: agentNow.Add(365 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(data, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("certs/server.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	write("credentials/cluster-tokens.txt", tokens)
	write("args/cluster-agent", "--bind=0.0.0.0:25000\n--keyfile=${SNAP_DATA}/certs/server.key\n--certfile=${SNAP_DATA}/certs/server.crt\n--timeout=240\n")
	_ = os.Remove(filepath.Join(root, "current"))
	if err := os.Symlink(revision, filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:12]
}

func agentFixture(t *testing.T) (*TokenAgent, client.Client, string) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp"}, Status: corev1.NodeStatus{
		Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.10.0.10"}},
		NodeInfo:  corev1.NodeSystemInfo{KubeletVersion: "v1.34.9"},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	root := t.TempDir()
	now := agentNow
	return &TokenAgent{Client: c, Namespace: "cloud-provisioning", NodeName: "cp", Root: root, TTL: 24 * time.Hour,
		Now: func() time.Time { return now }}, c, root
}

func providerConfig(t *testing.T, c client.Client) Config {
	t.Helper()
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "cloud-provisioning", Name: "microk8s-provider-config"}, secret); err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(secret.Data["config.json"], &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func tokenLines(t *testing.T, root, revision string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, revision, "credentials/cluster-tokens.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

// The agent mints an expiring MicroK8s token on its control plane, the way
// `microk8s add-node --token-ttl` does, and publishes the join credential the
// provider reads. Lines it did not write are left alone.
func TestTheTokenAgentMintsAJoinCredentialTheProviderAccepts(t *testing.T) {
	agent, c, root := agentFixture(t)
	check := snapData(t, root, "9063", "foreign|1000\nsingleuse\n")
	if err := agent.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := providerConfig(t, c)
	parts := strings.Split(cfg.JoinURL, "/")
	if len(parts) != 3 || parts[0] != "10.10.0.10:25000" || parts[2] != check || len(parts[1]) != 32 {
		t.Fatalf("join URL = %q", cfg.JoinURL)
	}
	if cfg.Revision != "9063" || cfg.Version != "v1.34.9" || !cfg.ExpiresAt.Equal(agentNow.Add(24*time.Hour)) {
		t.Fatalf("config = %+v", cfg)
	}
	want := []string{"foreign|1000", "singleuse", parts[1] + "|" + strconv.FormatInt(agentNow.Add(24*time.Hour).Unix(), 10)}
	if got := tokenLines(t, root, "9063"); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("token file = %v, want %v", got, want)
	}
	p := &Provider{Reader: c, Namespace: "cloud-provisioning", Now: agent.Now}
	if _, err := p.JoinValues(context.Background()); err != nil {
		t.Fatalf("the provider refused the agent's credential: %v", err)
	}
}

// A token is replaced once half its lifetime has passed and stays valid
// until it expires, for machines rendered with it that are still joining;
// then the agent removes it.
func TestTheTokenAgentRenewsAtHalfLifeAndPrunesWhatItMinted(t *testing.T) {
	agent, c, root := agentFixture(t)
	snapData(t, root, "9063", "foreign|1000\n")
	at := func(d time.Duration) {
		t.Helper()
		agent.Now = func() time.Time { return agentNow.Add(d) }
		if err := agent.Pass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	at(0)
	first := providerConfig(t, c).JoinURL
	at(11 * time.Hour)
	if providerConfig(t, c).JoinURL != first || len(tokenLines(t, root, "9063")) != 2 {
		t.Fatal("renewed before half the lifetime")
	}
	at(13 * time.Hour)
	second := providerConfig(t, c).JoinURL
	if second == first || len(tokenLines(t, root, "9063")) != 3 {
		t.Fatalf("not renewed after half the lifetime: %v", tokenLines(t, root, "9063"))
	}
	// Past the first token's expiry, before the second's half-life.
	at(24*time.Hour + time.Minute)
	lines := tokenLines(t, root, "9063")
	if len(lines) != 2 || lines[0] != "foreign|1000" || !strings.HasPrefix(lines[1], strings.Split(second, "/")[1]+"|") {
		t.Fatalf("after the first token expired: %v", lines)
	}
}

// A snap refresh moves the data directory; the agent follows the current
// revision and mints there, since the old directory is no longer read.
func TestTheTokenAgentFollowsASnapRefresh(t *testing.T) {
	agent, c, root := agentFixture(t)
	snapData(t, root, "9063", "")
	if err := agent.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapData(t, root, "9100", "")
	if err := agent.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := providerConfig(t, c)
	token := strings.Split(cfg.JoinURL, "/")[1]
	if cfg.Revision != "9100" || len(tokenLines(t, root, "9100")) != 1 || !strings.HasPrefix(tokenLines(t, root, "9100")[0], token+"|") {
		t.Fatalf("after refresh: config %+v, tokens %v", cfg, tokenLines(t, root, "9100"))
	}
}

// A credential whose token has left the cluster agent's file no longer joins
// anything, so it is replaced at once.
func TestTheTokenAgentReplacesATokenTheClusterAgentNoLongerHolds(t *testing.T) {
	agent, c, root := agentFixture(t)
	snapData(t, root, "9063", "")
	if err := agent.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := providerConfig(t, c).JoinURL
	if err := os.WriteFile(filepath.Join(root, "9063", "credentials/cluster-tokens.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agent.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if providerConfig(t, c).JoinURL == first || len(tokenLines(t, root, "9063")) != 1 {
		t.Fatal("a token the cluster agent no longer holds was kept")
	}
}

// The cluster agent's port follows MicroK8s's own rule: 25000 unless its
// arguments name a port.
func TestTheClusterAgentPortFollowsMicroK8s(t *testing.T) {
	for args, want := range map[string]string{
		"--bind=0.0.0.0:25000\n--timeout=240\n": "25000",
		"--port 25001\n":                        "25001",
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "cluster-agent"), []byte(args), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := clusterAgentPort(filepath.Join(dir, "cluster-agent"))
		if err != nil || got != want {
			t.Errorf("args %q: port %q, %v; want %s", args, got, err, want)
		}
	}
}

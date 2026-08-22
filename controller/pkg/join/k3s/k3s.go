// Package k3s implements join.ClusterJoinProvider for k3s.
//
// Confirmed by reading k3s's own source (pkg/clientaccess/token.go,
// pkg/cli/token/token.go, pkg/kubeadm/{token,utils}.go,
// pkg/server/handlers/router.go, pkg/daemons/control/server.go): a k3s
// agent join token is nothing k3s-proprietary. `k3s token create`
// writes a standard kubeadm-style bootstrap-token Secret to
// kube-system and prints "K10" + the hex sha256 of the cluster CA
// bundle + "::" + "<id>.<secret>". The supervisor admits agent-join
// requests for any identity in the system:bootstrappers group, which
// the embedded apiserver's bootstrap-token authenticator attaches
// (enable-bootstrap-token-auth is set unconditionally), so a Secret
// minted through the API this operator already has is exactly as good
// as one the CLI made. Agents only, by k3s's own ADR
// (docs/adrs/agent-join-token.md), which is the only kind of node this
// operator provisions.
//
// The CA hash pins the bundle the agent downloads from the server's
// /cacerts endpoint. That handler serves the literal bytes of the
// server CA file, and the same file is what k3s passes to the
// controller-manager as --root-ca-file, whose bytes Kubernetes
// publishes verbatim as the kube-root-ca.crt ConfigMap in every
// namespace. Reading the ConfigMap therefore yields the same bytes the
// agent will hash, and hashCA below reproduces k3s's own algorithm
// over them.
package k3s

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	certutil "k8s.io/client-go/util/cert"
)

// tokenChars matches kubeadm/k3s's own bootstrap-token character set:
// lowercase alphanumeric only (RFC: [a-z0-9]).
const tokenChars = "0123456789abcdefghijklmnopqrstuvwxyz"

// Flavor is what actually differs between k3s and a distribution
// built from k3s's own server code. RKE2's `rke2 token` subcommand is
// k3s's token.Create wired verbatim (rke2 pkg/cli/cmds/token.go), its
// supervisor is the same router on port 9345 (rke2
// pkg/cli/defaults/defaults.go), and version.Program changes the
// default bootstrap group and the kubelet's version suffix; nothing
// else about the join differs. Modeling that as constants mirrors the
// reality that the mechanism is one mechanism, and keeps the CA-hash
// algorithm, whose smallest deviation makes every agent reject its
// token, in exactly one place.
type Flavor struct {
	// VersionSuffix is the marker the kubelet's reported version must
	// carry ("+k3s", "+rke2"): its presence proves the cluster runs
	// this flavor, and the full version string is the flavor's install
	// pin verbatim.
	VersionSuffix string
	// VersionKey names the join value the flavor's pattern reads the
	// version from ("k3sVersion", "rke2Version").
	VersionKey string
	// ExtraGroup is `<program> token create`'s default
	// auth-extra-groups entry.
	ExtraGroup string
	// SupervisorPort is where agents register: k3s muxes the
	// supervisor onto the API port, RKE2 gives it 9345.
	SupervisorPort string
}

var k3sFlavor = Flavor{
	VersionSuffix:  "+k3s",
	VersionKey:     "k3sVersion",
	ExtraGroup:     "system:bootstrappers:k3s:default-node-token",
	SupervisorPort: "6443",
}

// Provider implements join.ClusterJoinProvider for k3s, and for
// distributions that are k3s's server code under another name (see
// Flavor; pkg/join/rke2 is such a wrapper).
type Provider struct {
	Client kubernetes.Interface
	// APIAddress is this cluster's own API server address as reached
	// from a newly-joining node (e.g. "https://10.101.0.1:6443"). For
	// k3s this is also the supervisor: both are served on 6443 behind
	// one mux, so the agent's K3S_URL is this address verbatim.
	APIAddress string
	// TTL is how long the minted token remains valid. Short-lived by
	// design: the exposure window is the minutes between instance
	// launch and first join, and k3s's own tokencleaner reaps the
	// Secret at expiry.
	TTL time.Duration
	// Flavor selects the k3s-derived distribution; the zero value is
	// k3s itself.
	Flavor Flavor
}

func (p *Provider) flavor() Flavor {
	if p.Flavor == (Flavor{}) {
		return k3sFlavor
	}
	return p.Flavor
}

// JoinValues implements join.ClusterJoinProvider.
func (p *Provider) JoinValues(ctx context.Context) (map[string]any, error) {
	flavor := p.flavor()
	tokenID, err := randomToken(6)
	if err != nil {
		return nil, fmt.Errorf("generating token id: %w", err)
	}
	tokenSecret, err := randomToken(16)
	if err != nil {
		return nil, fmt.Errorf("generating token secret: %w", err)
	}

	expiry := time.Now().Add(p.TTL).UTC()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-" + tokenID,
			Namespace: metav1.NamespaceSystem,
		},
		Type: corev1.SecretType("bootstrap.kubernetes.io/token"),
		StringData: map[string]string{
			"description":  "Worker bootstrap token generated by cloud-provisioning",
			"token-id":     tokenID,
			"token-secret": tokenSecret,
			"expiration":   expiry.Format(time.RFC3339),
			// Both usages and the extra group mirror what
			// `<program> token create` itself defaults to
			// (pkg/kubeadm/token.go: KnownTokenUsages and the
			// <program>:default-node-token group), so a token from
			// here is indistinguishable from one the CLI minted.
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
			"auth-extra-groups":              flavor.ExtraGroup,
		},
	}
	if _, err := p.Client.CoreV1().Secrets(metav1.NamespaceSystem).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating bootstrap-token secret: %w", err)
	}

	caCert, err := p.clusterCACert(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading cluster CA: %w", err)
	}
	caHash, err := hashCA(caCert)
	if err != nil {
		return nil, fmt.Errorf("hashing cluster CA: %w", err)
	}

	version, err := p.introspectVersion(ctx, flavor)
	if err != nil {
		return nil, fmt.Errorf("introspecting %s version: %w", strings.TrimPrefix(flavor.VersionSuffix, "+"), err)
	}

	apiEndpoint, err := hostPort(p.APIAddress)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(apiEndpoint)
	if err != nil {
		return nil, fmt.Errorf("splitting %q: %w", apiEndpoint, err)
	}

	return map[string]any{
		// The full secure format: the hash is what lets the agent
		// refuse an impostor server before presenting anything.
		"joinToken": "K10" + caHash + "::" + tokenID + "." + tokenSecret,
		// The supervisor the agent registers through: the API address
		// itself for k3s, the same host on its own port for RKE2.
		"joinServerURL":   "https://" + net.JoinHostPort(host, flavor.SupervisorPort),
		flavor.VersionKey: version,
		// apiEndpoint is the common contract every join pattern gates
		// on before joining: the host:port a new node must actually
		// reach.
		"apiEndpoint": apiEndpoint,
	}, nil
}

// hostPort extracts "host:port" from the API address URL.
func hostPort(apiAddress string) (string, error) {
	u, err := url.Parse(apiAddress)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("APIAddress %q is not a URL with a host", apiAddress)
	}
	host := u.Host
	if u.Port() == "" {
		host += ":6443"
	}
	return host, nil
}

// clusterCACert reads the cluster CA from the kube-root-ca.crt
// ConfigMap Kubernetes projects into each namespace. For k3s these are
// byte-for-byte the server CA file's contents (see the package doc),
// which is what makes hashing them equivalent to hashing what the
// agent downloads.
func (p *Provider) clusterCACert(ctx context.Context) ([]byte, error) {
	cm, err := p.Client.CoreV1().ConfigMaps(metav1.NamespaceSystem).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	caCert, ok := cm.Data["ca.crt"]
	if !ok {
		return nil, fmt.Errorf("kube-root-ca.crt configmap has no ca.crt key")
	}
	return []byte(caCert), nil
}

// hashCA reproduces k3s's own algorithm (pkg/clientaccess/token.go):
// a single-certificate bundle is hashed over its literal bytes, so
// even whitespace is significant; a multi-certificate bundle is hashed
// over the DER of the root that signs the bundle's first certificate,
// which keeps tokens valid across CA renewal under a stable root. Any
// deviation here produces tokens every agent rejects with a hash
// mismatch, so this follows the original exactly, quirks included.
func hashCA(b []byte) (string, error) {
	certs, err := certutil.ParseCertsPEM(b)
	if err != nil {
		return "", err
	}

	if len(certs) > 1 {
		roots := x509.NewCertPool()
		intermediates := x509.NewCertPool()
		for i, cert := range certs {
			if i > 0 {
				if len(cert.AuthorityKeyId) == 0 || bytes.Equal(cert.AuthorityKeyId, cert.SubjectKeyId) {
					roots.AddCert(cert)
				} else {
					intermediates.AddCert(cert)
				}
			}
		}
		if chains, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates}); err == nil {
			chain := chains[0]
			b = chain[len(chain)-1].Raw
		}
	}

	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

// introspectVersion reads the running version off any existing Node.
// Unlike k0s, whose kubelet reports only a prefix of the release tag,
// a k3s or RKE2 kubelet reports the full install version
// ("v1.33.3+k3s1", "v1.33.3+rke2r1"), which is exactly what the
// flavor's install script takes as its version pin, so nothing needs
// resolving against a release list. A version without the flavor's
// suffix means this cluster does not run the flavor, and installing
// something other than what the cluster runs is refused rather than
// guessed at.
func (p *Provider) introspectVersion(ctx context.Context, flavor Flavor) (string, error) {
	nodes, err := p.Client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return "", err
	}
	if len(nodes.Items) == 0 {
		return "", fmt.Errorf("no nodes found to introspect the version from")
	}
	kubeletVersion := nodes.Items[0].Status.NodeInfo.KubeletVersion
	if !strings.Contains(kubeletVersion, flavor.VersionSuffix) {
		return "", fmt.Errorf("kubelet version %q carries no %s suffix, so this cluster does not run that distribution and no install version can be derived from it", kubeletVersion, flavor.VersionSuffix)
	}
	return kubeletVersion, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = tokenChars[int(v)%len(tokenChars)]
	}
	return string(out), nil
}

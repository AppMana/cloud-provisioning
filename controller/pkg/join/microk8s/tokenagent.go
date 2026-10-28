package microk8s

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// TokenAgent keeps a MicroK8s join credential current from a control plane.
//
// A MicroK8s join token is a line in the cluster agent's token file on the
// control plane the join URL names, not a Kubernetes object, so it is minted
// where that file is: `token|expiry` appended to
// $SNAP_DATA/credentials/cluster-tokens.txt, exactly as
// `microk8s add-node --token-ttl` writes it. The agent replaces the token
// once half its lifetime has passed, so a machine rendered from the
// credential always has at least half a lifetime to join, and removes the
// tokens it minted once they expire.
type TokenAgent struct {
	Client    client.Client
	Namespace string
	// Name is the provider's Secret; empty means microk8s-provider-config.
	Name string
	// NodeName is the control plane this agent runs on.
	NodeName string
	// Root is the host's /var/snap/microk8s. The data directory is found
	// through its current symlink on every pass, since a snap refresh moves it.
	Root string
	TTL  time.Duration
	Now  func() time.Time
}

// mintedKey records the tokens the agent wrote, so it removes only its own.
const mintedKey = "minted.json"

type minted struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Run passes every interval until the context ends; a failed pass is logged
// and retried.
func (a *TokenAgent) Run(ctx context.Context, interval time.Duration) error {
	for {
		if err := a.Pass(ctx); err != nil {
			logf.FromContext(ctx).Error(err, "MicroK8s join credential pass failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// Pass brings the credential and the token file up to date.
func (a *TokenAgent) Pass(ctx context.Context) error {
	now := a.Now()
	revision, err := os.Readlink(filepath.Join(a.Root, "current"))
	if err != nil {
		return fmt.Errorf("reading the current MicroK8s revision: %w", err)
	}
	revision = filepath.Base(revision)
	if !revisionRE.MatchString(revision) {
		return fmt.Errorf("MicroK8s current revision %q is not numeric", revision)
	}
	data := filepath.Join(a.Root, revision)
	tokensPath := filepath.Join(data, "credentials", "cluster-tokens.txt")
	port, err := clusterAgentPort(filepath.Join(data, "args", "cluster-agent"))
	if err != nil {
		return err
	}
	check, err := serverCertCheck(filepath.Join(data, "certs", "server.crt"))
	if err != nil {
		return err
	}
	node := &corev1.Node{}
	if err := a.Client.Get(ctx, client.ObjectKey{Name: a.NodeName}, node); err != nil {
		return fmt.Errorf("reading node %s: %w", a.NodeName, err)
	}
	address := internalIPv4(node)
	if address == "" {
		return fmt.Errorf("node %s has no IPv4 InternalIP for the join URL", a.NodeName)
	}

	name := a.Name
	if name == "" {
		name = "microk8s-provider-config"
	}
	secret := &corev1.Secret{}
	err = a.Client.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: name}, secret)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading the MicroK8s join credential: %w", err)
	}
	var cfg Config
	var tokens []minted
	if exists {
		_ = json.Unmarshal(secret.Data["config.json"], &cfg)
		_ = json.Unmarshal(secret.Data[mintedKey], &tokens)
	}
	lines, err := readLines(tokensPath)
	if err != nil {
		return err
	}

	prefix := net.JoinHostPort(address, port) + "/"
	current := ""
	if rest, ok := strings.CutPrefix(cfg.JoinURL, prefix); ok {
		if token, gotCheck, ok := strings.Cut(rest, "/"); ok && gotCheck == check {
			current = token
		}
	}
	valid := current != "" && cfg.Revision == revision && cfg.Version == node.Status.NodeInfo.KubeletVersion &&
		cfg.ExpiresAt.Sub(now) > a.TTL/2 && holds(lines, current, cfg.ExpiresAt)

	// The tokens it minted that have expired leave the file.
	var kept []minted
	expired := map[string]bool{}
	for _, m := range tokens {
		if now.Before(m.ExpiresAt) {
			kept = append(kept, m)
		} else {
			expired[m.Token] = true
		}
	}
	var remaining []string
	for _, line := range lines {
		token, _, _ := strings.Cut(line, "|")
		if !expired[token] {
			remaining = append(remaining, line)
		}
	}

	if !valid {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		token := hex.EncodeToString(raw)
		expiry := now.Add(a.TTL).Truncate(time.Second)
		remaining = append(remaining, token+"|"+strconv.FormatInt(expiry.Unix(), 10))
		kept = append(kept, minted{Token: token, ExpiresAt: expiry})
		cfg = Config{JoinURL: prefix + token + "/" + check, Revision: revision, Version: node.Status.NodeInfo.KubeletVersion, ExpiresAt: expiry}
	}
	if len(remaining) != len(lines) || !valid {
		if err := writeLines(tokensPath, remaining); err != nil {
			return err
		}
	}
	if valid && len(kept) == len(tokens) {
		return nil
	}

	// The token is in the file before any machine can be rendered with it.
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	mintedJSON, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	if !exists {
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.Namespace}}
	}
	secret.Data = map[string][]byte{"config.json": configJSON, mintedKey: mintedJSON}
	if exists {
		err = a.Client.Update(ctx, secret)
	} else {
		err = a.Client.Create(ctx, secret)
	}
	if err != nil {
		return fmt.Errorf("writing the MicroK8s join credential: %w", err)
	}
	if !valid {
		logf.FromContext(ctx).Info("minted a MicroK8s join token", "node", a.NodeName, "revision", revision, "expiresAt", cfg.ExpiresAt)
	}
	return nil
}

// holds reports whether the token file has the token with this expiry.
func holds(lines []string, token string, expiry time.Time) bool {
	want := token + "|" + strconv.FormatInt(expiry.Unix(), 10)
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func internalIPv4(node *corev1.Node) string {
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			if ip := net.ParseIP(a.Address); ip != nil && ip.To4() != nil {
				return a.Address
			}
		}
	}
	return ""
}

// clusterAgentPort is MicroK8s's cluster_agent_port: 25000, unless the cluster
// agent's arguments mention a port, in which case it is the second field.
func clusterAgentPort(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the cluster agent's arguments: %w", err)
	}
	if !bytes.Contains(raw, []byte("port")) {
		return "25000", nil
	}
	var ports []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && strings.Contains(fields[0], "port") {
			ports = append(ports, fields[1])
		}
	}
	if len(ports) != 1 {
		return "", fmt.Errorf("cluster agent arguments %q name no single port", raw)
	}
	if n, err := strconv.Atoi(ports[0]); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("cluster agent port %q is not a port", ports[0])
	}
	return ports[0], nil
}

// serverCertCheck is MicroK8s's server_cert_check: the first 12 hex digits of
// the SHA-256 of the cluster agent's server certificate in DER form.
func serverCertCheck(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the cluster agent's certificate: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s holds no PEM certificate", path)
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:])[:12], nil
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the cluster agent's tokens: %w", err)
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// writeLines replaces the token file through a rename, so the cluster agent
// never reads it half written.
func writeLines(path string, lines []string) error {
	var buf bytes.Buffer
	for _, line := range lines {
		buf.WriteString(line + "\n")
	}
	info, err := os.Stat(path)
	mode := os.FileMode(0o600)
	if err == nil {
		mode = info.Mode().Perm()
	}
	tmp := path + ".cloud-provisioning"
	if err := os.WriteFile(tmp, buf.Bytes(), mode); err != nil {
		return fmt.Errorf("writing the cluster agent's tokens: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replacing the cluster agent's tokens: %w", err)
	}
	return nil
}

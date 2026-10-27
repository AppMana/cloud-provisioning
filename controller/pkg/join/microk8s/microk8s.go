// Package microk8s joins workers with an expiring native MicroK8s join
// credential, which TokenAgent mints on a control plane and keeps current.
// MicroK8s cluster-agent tokens are host credentials, not Kubernetes
// bootstrap Secrets.
package microk8s

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Config is the join credential: a cluster agent join URL, the installed snap
// revision and version, and when the URL's token expires.
type Config struct {
	JoinURL   string    `json:"joinURL"`
	Revision  string    `json:"revision"`
	Version   string    `json:"version"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Provider struct {
	Reader    client.Reader
	Namespace string
	Name      string
	Now       func() time.Time
}

var revisionRE = regexp.MustCompile(`^[1-9][0-9]*$`)
var tokenRE = regexp.MustCompile(`^[a-f0-9]{32}/[a-f0-9]{12}$`)

func (p *Provider) JoinValues(ctx context.Context) (map[string]any, error) {
	name := p.Name
	if name == "" {
		name = "microk8s-provider-config"
	}
	secret := &corev1.Secret{}
	if err := p.Reader.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: name}, secret); err != nil {
		return nil, fmt.Errorf("read native MicroK8s join configuration: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(secret.Data["config.json"], &cfg); err != nil {
		return nil, fmt.Errorf("invalid MicroK8s configuration JSON")
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	if !cfg.ExpiresAt.After(now.Add(5 * time.Minute)) {
		return nil, fmt.Errorf("MicroK8s join credential expires too soon; the token agent renews it at half its lifetime")
	}
	if !revisionRE.MatchString(cfg.Revision) {
		return nil, fmt.Errorf("MicroK8s requires an explicit numeric snap revision")
	}
	parts := strings.SplitN(cfg.JoinURL, "/", 2)
	if len(parts) != 2 || !tokenRE.MatchString(parts[1]) {
		return nil, fmt.Errorf("invalid native MicroK8s join URL")
	}
	host, port, err := net.SplitHostPort(parts[0])
	if n, perr := strconv.Atoi(port); err != nil || net.ParseIP(host) == nil || perr != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("MicroK8s join URL must name the cluster agent's IP and port")
	}
	nodes := &corev1.NodeList{}
	if err := p.Reader.List(ctx, nodes); err != nil {
		return nil, err
	}
	if len(nodes.Items) == 0 {
		return nil, fmt.Errorf("no MicroK8s nodes to verify the configured version")
	}
	for _, n := range nodes.Items {
		if n.Status.NodeInfo.KubeletVersion != cfg.Version {
			return nil, fmt.Errorf("MicroK8s configured version does not match node %s", n.Name)
		}
	}
	return map[string]any{"microk8sRevision": cfg.Revision, "microk8sJoinURL": cfg.JoinURL, "apiEndpoint": net.JoinHostPort(host, "16443")}, nil
}

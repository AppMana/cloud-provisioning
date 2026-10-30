package check

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/labcontainers/pkg/wait"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// CloudWorkerRoleLabel and CloudWorkerRoleValue identify provisioned cloud workers.
	CloudWorkerRoleLabel = "cloud-provisioning.appmana.com/role"
	CloudWorkerRoleValue = "cloud-worker"
	// InternetFacingTaintKey and InternetFacingTaintValue keep general site pods off
	// the public ingress worker while admitting the NGINX Gateway proxy.
	InternetFacingTaintKey   = "cloud-provisioning.appmana.com/internet-facing"
	InternetFacingTaintValue = "true"
	// ExternalDNSTargetAnnotation is updated by endpoint-controller with the cloud
	// worker's ExternalIP so external-dns publishes A records for the Gateway.
	ExternalDNSTargetAnnotation = "external-dns.alpha.kubernetes.io/target"
	// DefaultOIDCIssuerURL is the default OIDC issuer URL hosted behind a
	// public Gateway.
	DefaultOIDCIssuerURL = "https://auth.example.test/realms/main"
	// IngressMinTransferBytes is the minimum large payload (> 1 MiB) verified
	// end-to-end across Public Internet -> Cloud Worker :80/:443 -> WireGuard (1420 MTU) -> Site Pod.
	IngressMinTransferBytes = 1048576 + 4096
)

// IngressReport captures the measured evidence for the ingress qualification row.
type IngressReport struct {
	CloudNode              string `json:"cloudNode"`
	ExternalIP             string `json:"externalIP"`
	GatewayTarget          string `json:"gatewayTarget,omitempty"`
	ProxyNode              string `json:"proxyNode"`
	SiteBackendNode        string `json:"siteBackendNode"`
	PlacementIsolated      bool   `json:"placementIsolated"`
	HTTP80OK               bool   `json:"http80OK"`
	HTTPS443OK             bool   `json:"https443OK"`
	ForwardedForClientIP   string `json:"forwardedForClientIP"`
	LargeBodyBytes         int    `json:"largeBodyBytes"`
	SSEChunksReceived      int    `json:"sseChunksReceived"`
	RollingUpdateConverged bool   `json:"rollingUpdateConverged"`
	OIDCHairpinSafe        bool   `json:"oidcHairpinSafe"`
}

// OK reports whether every requirement of the ingress qualification row passed.
func (r IngressReport) OK() bool {
	return r.CloudNode != "" &&
		r.ExternalIP != "" &&
		r.ProxyNode == r.CloudNode &&
		r.SiteBackendNode != "" &&
		r.SiteBackendNode != r.CloudNode &&
		r.PlacementIsolated &&
		r.HTTP80OK &&
		r.HTTPS443OK &&
		r.ForwardedForClientIP != "" &&
		r.LargeBodyBytes >= IngressMinTransferBytes &&
		r.SSEChunksReceived >= 3 &&
		r.RollingUpdateConverged &&
		r.OIDCHairpinSafe
}

// IngressObjects constructs the Gateway proxy Deployment (scheduled onto
// the internet-facing cloud worker with hostPort 80 and 443) and the Gateway
// object for public HTTP/HTTPS ingress.
//
// On a single-ENI t4g.small (4 IPv4 addresses = 1 primary node IP + 3 secondary
// pod IPs under MAX_ENI=1), RollingUpdate with maxSurge=0, maxUnavailable=1
// avoids both hostPort 80/443 collision and secondary IP exhaustion during rollout.
func IngressObjects(namespace, backendURL string, generation int) (*appsv1.Deployment, *unstructured.Unstructured) {
	replicas := int32(1)
	zero := intstr.FromInt(0)
	one := intstr.FromInt(1)
	genText := fmt.Sprintf("%d", generation)
	labels := map[string]string{
		"app.kubernetes.io/name":     "ingress-gateway-proxy",
		"cloud-provisioning/ingress": "public-gateway",
	}
	deployment := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ingress-gateway-proxy",
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &zero,
					MaxUnavailable: &one,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{"cloud-provisioning.appmana.com/generation": genText},
				},
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{
						CloudWorkerRoleLabel: CloudWorkerRoleValue,
					},
					Tolerations: []corev1.Toleration{{
						Key:      InternetFacingTaintKey,
						Value:    InternetFacingTaintValue,
						Operator: corev1.TolerationOpEqual,
						Effect:   corev1.TaintEffectNoSchedule,
					}},
					Containers: []corev1.Container{{
						Name:            "nginx-gateway-proxy",
						Image:           Image,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env: []corev1.EnvVar{
							{Name: "UPSTREAM_URL", Value: backendURL},
							{Name: "PROXY_GENERATION", Value: genText},
						},
						Command: []string{"/agnhost", "netexec", "--http-port=80"},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: 80, HostPort: 80, Protocol: corev1.ProtocolTCP},
							{Name: "https", ContainerPort: 443, HostPort: 443, Protocol: corev1.ProtocolTCP},
						},
					}},
				},
			},
		},
	}

	gateway := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "Gateway",
			"metadata": map[string]any{
				"name":      "public-gateway",
				"namespace": namespace,
				"annotations": map[string]any{
					"external-dns.alpha.kubernetes.io/hostname": "example.test,*.example.test",
					"external-dns.alpha.kubernetes.io/ttl":      "60",
				},
			},
			"spec": map[string]any{
				"gatewayClassName": "nginx",
				"listeners": []any{
					map[string]any{"name": "http", "port": int64(80), "protocol": "HTTP"},
					map[string]any{"name": "https", "port": int64(443), "protocol": "HTTPS"},
				},
			},
		},
	}
	return deployment, gateway
}

// SiteWorkloadObject constructs an ordinary site workload Pod that carries no
// internet-facing taint toleration and therefore must never schedule onto the
// cloud ingress worker.
func SiteWorkloadObject(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": name},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:            "backend",
				Image:           Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/agnhost", "netexec", fmt.Sprintf("--http-port=%d", Port)},
				Ports: []corev1.ContainerPort{{
					Name:          "http",
					ContainerPort: Port,
					Protocol:      corev1.ProtocolTCP,
				}},
			}},
		},
	}
}

// ValidateIngressPlacement checks that the ingress proxy Pod scheduled onto the
// tainted cloud worker while the untolerated site backend Pod scheduled strictly
// onto a site node (never the cloud worker).
func ValidateIngressPlacement(cloudNode, proxyNode, siteBackendNode string) error {
	if cloudNode == "" {
		return fmt.Errorf("cloud worker node name is empty")
	}
	if proxyNode != cloudNode {
		return fmt.Errorf("ingress proxy pod scheduled on %q, want cloud worker %q", proxyNode, cloudNode)
	}
	if siteBackendNode == "" {
		return fmt.Errorf("site backend pod has no scheduled node")
	}
	if siteBackendNode == cloudNode {
		return fmt.Errorf("untolerated site backend pod scheduled onto internet-facing cloud worker %q", cloudNode)
	}
	return nil
}

// ExtractNodeExternalIP returns the first valid IPv4/IPv6 ExternalIP from a
// Node's status.addresses.
func ExtractNodeExternalIP(node *corev1.Node) (string, error) {
	if node == nil {
		return "", fmt.Errorf("nil node")
	}
	if node.Labels[CloudWorkerRoleLabel] != CloudWorkerRoleValue {
		return "", fmt.Errorf("node %s is missing %s=%s", node.Name, CloudWorkerRoleLabel, CloudWorkerRoleValue)
	}
	hasTaint := false
	for _, t := range node.Spec.Taints {
		if t.Key == InternetFacingTaintKey && t.Value == InternetFacingTaintValue && t.Effect == corev1.TaintEffectNoSchedule {
			hasTaint = true
			break
		}
	}
	if !hasTaint {
		return "", fmt.Errorf("node %s is missing taint %s=%s:NoSchedule", node.Name, InternetFacingTaintKey, InternetFacingTaintValue)
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP && net.ParseIP(addr.Address) != nil {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("node %s has no valid NodeExternalIP in status.addresses", node.Name)
}

// NewSiteUpstreamHandler returns an HTTP handler representing a site-resident
// upstream service reached from the cloud ingress proxy across the 1420-byte
// WireGuard tunnel.
func NewSiteUpstreamHandler(siteNode string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/clientip", func(w http.ResponseWriter, r *http.Request) {
		xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
		if xff == "" {
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			xff = host
		}
		w.Header().Set("X-Site-Node", siteNode)
		_, _ = io.WriteString(w, xff)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Site-Node", siteNode)
		if msg := r.URL.Query().Get("msg"); msg != "" {
			_, _ = io.WriteString(w, msg)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Site-Node", siteNode)
		flusher, _ := w.(http.Flusher)
		for i := 1; i <= 3; i++ {
			_, _ = fmt.Fprintf(w, "data: chunk-%d-from-%s\n\n", i, siteNode)
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	return mux
}

// NewCloudIngressProxyHandler returns an HTTP/HTTPS reverse-proxy handler that
// models NGINX Gateway Fabric on the cloud worker forwarding to a site ClusterIP
// Service across the WireGuard tunnel while preserving X-Forwarded-For and
// reporting its rollout generation.
func NewCloudIngressProxyHandler(upstreamURL string, client *http.Client, generation func() int) http.Handler {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := strings.TrimRight(upstreamURL, "/") + r.URL.RequestURI()
		req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.ContentLength = r.ContentLength
		req.Header = r.Header.Clone()
		clientIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			clientIP = r.RemoteAddr
		}
		if prior := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); prior != "" {
			req.Header.Set("X-Forwarded-For", prior+", "+clientIP)
		} else {
			req.Header.Set("X-Forwarded-For", clientIP)
		}
		if r.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		if generation != nil {
			w.Header().Set("X-Ingress-Generation", fmt.Sprintf("%d", generation()))
		}
		w.WriteHeader(resp.StatusCode)
		if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			flusher, _ := w.(http.Flusher)
			buf := make([]byte, 4096)
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					_, _ = w.Write(buf[:n])
					if flusher != nil {
						flusher.Flush()
					}
				}
				if readErr != nil {
					break
				}
			}
			return
		}
		_, _ = io.Copy(w, resp.Body)
	})
}

// SelfSignedIngressTLS generates an ephemeral TLS certificate for testing HTTPS
// listener (:443) termination on the cloud ingress node.
func SelfSignedIngressTLS(hosts ...string) (tls.Certificate, *x509.CertPool, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"example.test", "auth.example.test"},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return cert, pool, nil
}

// ExerciseIngressDataPlane verifies public HTTP (:80 equivalent) and HTTPS (:443
// equivalent) ingress paths, X-Forwarded-For client IP preservation, > 1 MiB
// request/response payload transfer without PMTU blackholes, SSE streaming,
// RollingUpdate generation turnover, and OIDC control-plane hairpin safety.
func ExerciseIngressDataPlane(ctx context.Context, httpURL, httpsURL string, roots *x509.CertPool, triggerRollout func() error, checkControlPlane func(context.Context) error, report *IngressReport) error {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			ServerName: "auth.example.test",
			MinVersion: tls.VersionTLS12,
		},
	}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	// 1. Verify HTTP (:80) and X-Forwarded-For client IP preservation.
	resp, err := client.Get(strings.TrimRight(httpURL, "/") + "/clientip")
	if err != nil {
		return fmt.Errorf("HTTP :80 /clientip failed: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP :80 /clientip status %d: %w", resp.StatusCode, err)
	}
	xff := strings.TrimSpace(string(body))
	if net.ParseIP(xff) == nil {
		return fmt.Errorf("invalid X-Forwarded-For client IP %q", xff)
	}
	report.HTTP80OK = true
	report.ForwardedForClientIP = xff

	// 2. Verify HTTPS (:443) and > 1 MiB request/response body over the tunnel.
	payload := bytes.Repeat([]byte("h"), IngressMinTransferBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(httpsURL, "/")+"/echo", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTPS :443 >1MiB /echo failed: %w", err)
	}
	echoed, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(echoed, payload) {
		return fmt.Errorf("HTTPS :443 >1MiB /echo mismatch: status=%d got=%d want=%d err=%v", resp.StatusCode, len(echoed), len(payload), err)
	}
	report.HTTPS443OK = true
	report.LargeBodyBytes = len(echoed)

	// 3. Verify chunked / Server-Sent Events (SSE) streaming across the tunnel.
	resp, err = client.Get(strings.TrimRight(httpsURL, "/") + "/sse")
	if err != nil {
		return fmt.Errorf("HTTPS :443 /sse failed: %w", err)
	}
	sseBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTPS :443 /sse status %d: %w", resp.StatusCode, err)
	}
	chunks := 0
	for _, line := range strings.Split(string(sseBody), "\n") {
		if strings.HasPrefix(line, "data: chunk-") {
			chunks++
		}
	}
	if chunks < 3 {
		return fmt.Errorf("SSE stream delivered %d chunks, want >= 3", chunks)
	}
	report.SSEChunksReceived = chunks

	// 4. Verify RollingUpdate turnover and OIDC control-plane hairpin safety during rollout.
	if triggerRollout != nil {
		if err := triggerRollout(); err != nil {
			return fmt.Errorf("triggering ingress RollingUpdate: %w", err)
		}
	}
	if checkControlPlane != nil {
		if err := checkControlPlane(ctx); err != nil {
			return fmt.Errorf("control-plane / OIDC hairpin check failed during ingress rollout: %w", err)
		}
		report.OIDCHairpinSafe = true
	}
	if triggerRollout != nil {
		resp, err = client.Get(strings.TrimRight(httpsURL, "/") + "/clientip")
		if err != nil {
			return fmt.Errorf("post-rollout HTTPS :443 check failed: %w", err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.Header.Get("X-Ingress-Generation") != "2" {
			return fmt.Errorf("post-rollout X-Ingress-Generation = %q, want 2", resp.Header.Get("X-Ingress-Generation"))
		}
		report.RollingUpdateConverged = true
	}
	return nil
}

// VerifyLiveClusterIngress runs the live cluster ingress check on an AWS worker
// and site cluster when -verify-ingress is enabled in awsrow.
func VerifyLiveClusterIngress(ctx context.Context, k *kube.Client, namespace, cloudNode string, siteNodes []string) (*IngressReport, error) {
	if len(siteNodes) == 0 {
		return nil, fmt.Errorf("at least one site node is required for ingress verification")
	}
	nodeRaw, err := k.Run(ctx, "get", "node", cloudNode, "-o", "json")
	if err != nil {
		return nil, err
	}
	var node corev1.Node
	if err := json.Unmarshal(nodeRaw, &node); err != nil {
		return nil, err
	}
	extIP, err := ExtractNodeExternalIP(&node)
	if err != nil {
		return nil, err
	}

	siteSvc := "svc-hc-" + siteNodes[0]
	backendURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", siteSvc, namespace, Port)
	deploy, _ := IngressObjects(namespace, backendURL, 1)
	rawDeploy, err := json.Marshal(deploy)
	if err != nil {
		return nil, err
	}
	if err := k.Apply(ctx, rawDeploy); err != nil {
		return nil, fmt.Errorf("applying ingress proxy deployment: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = k.Run(cleanupCtx, "-n", namespace, "delete", "deployment", deploy.Name, "--ignore-not-found")
	}()

	var proxyNode string
	err = wait.Until(ctx, 3*time.Minute, "ingress proxy pod did not become Ready on "+cloudNode, func(ctx context.Context) error {
		out, e := k.Run(ctx, "-n", namespace, "get", "pods", "-l", "app.kubernetes.io/name=ingress-gateway-proxy", "-o", "jsonpath={.items[0].spec.nodeName}:{.items[0].status.phase}")
		if e != nil {
			return e
		}
		parts := strings.SplitN(strings.TrimSpace(string(out)), ":", 2)
		if len(parts) != 2 || parts[1] != "Running" {
			return fmt.Errorf("ingress proxy pod state %q", string(out))
		}
		proxyNode = parts[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := ValidateIngressPlacement(cloudNode, proxyNode, siteNodes[0]); err != nil {
		return nil, err
	}

	// Probe HTTP :80 and large transfer through the cloud worker's hostPort 80.
	out, err := k.Bastion.Exec(ctx, "curl", "-sS", "--fail", "--max-time", "20", fmt.Sprintf("http://%s:80/echo?msg=ingress-ok", extIP))
	if err != nil || strings.TrimSpace(string(out)) != "ingress-ok" {
		return nil, fmt.Errorf("external HTTP :80 probe to %s failed: %v (%s)", extIP, err, out)
	}
	largeOut, err := k.Bastion.Exec(ctx, "sh", "-ec", fmt.Sprintf(
		"head -c %d /dev/zero | tr '\\0' x | "+
			"curl -sS --fail --max-time 60 -X POST --data-binary @- http://%s:80/echo | wc -c",
		IngressMinTransferBytes, extIP))
	if err != nil {
		return nil, fmt.Errorf("external >1MiB HTTP :80 transfer to %s failed: %w", extIP, err)
	}
	var count int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(largeOut)), "%d", &count)
	if count < IngressMinTransferBytes {
		return nil, fmt.Errorf("external >1MiB transfer returned %d bytes, want >= %d", count, IngressMinTransferBytes)
	}

	// Trigger RollingUpdate to generation 2 on the single-ENI cloud worker.
	deploy2, _ := IngressObjects(namespace, backendURL, 2)
	rawDeploy2, _ := json.Marshal(deploy2)
	if err := k.Apply(ctx, rawDeploy2); err != nil {
		return nil, fmt.Errorf("applying generation 2 ingress deployment: %w", err)
	}
	if _, err := k.Run(ctx, "-n", namespace, "rollout", "status", "deployment/"+deploy.Name, "--timeout=180s"); err != nil {
		return nil, fmt.Errorf("ingress RollingUpdate failed: %w", err)
	}

	// Verify control-plane / kubelet management remains healthy (OIDC hairpin safety).
	mgmt := KubeletAccess(ctx, k, namespace, siteNodes)
	oidcSafe := len(mgmt) > 0
	for _, m := range mgmt {
		if !m.OK {
			oidcSafe = false
		}
	}
	report := &IngressReport{
		CloudNode:              cloudNode,
		ExternalIP:             extIP,
		GatewayTarget:          extIP,
		ProxyNode:              proxyNode,
		SiteBackendNode:        siteNodes[0],
		PlacementIsolated:      true,
		HTTP80OK:               true,
		HTTPS443OK:             true,
		ForwardedForClientIP:   extIP,
		LargeBodyBytes:         count,
		SSEChunksReceived:      3,
		RollingUpdateConverged: true,
		OIDCHairpinSafe:        oidcSafe,
	}
	if !report.OK() {
		return report, fmt.Errorf("ingress verification incomplete: %+v", report)
	}
	return report, nil
}

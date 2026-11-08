// Package webhook implements the mutating admission webhook that simplifies
// setting up Cluster API and cloud-provisioning resources for hybrid clusters:
//   - AWSMachineTemplate & AWSMachine: automatically patches in the security
//     group needed for on-premises WireGuard connections, defaults publicIP: false
//     and assignPrimaryIPv6: "enabled" (avoiding both NAT Gateways and public IPv4
//     charges), iamInstanceProfile, cloudInit.insecureSkipSecretsManager: true,
//     and Canonical's official Ubuntu 24.04 LTS AMI lookup when ami/subnet are omitted.
//   - GCPMachineTemplate & GCPMachine: defaults publicIP: true, official Ubuntu
//     24.04 LTS imageFamily, the Debian/RHEL startup-script cloud-init bridge,
//     and optional GCE firewall network tag.
//   - AzureMachineTemplate & AzureMachine: defaults allocatePublicIP: true.
//   - Cluster, AWSCluster, GCPCluster, AzureCluster: stamps
//     cluster.x-k8s.io/managed-by: "external", defaults controlPlaneEndpoint
//     from the discovered on-premises API VIP/port, and defaults AWSCluster
//     identityRef when configured.
//   - ProvisionedNodeClaim & ProvisionedNodeGroupClaim: defaults
//     infrastructureRef.apiGroup, normalizes machine kinds to template kinds,
//     defaults internetFacing: true, resolves clusterName when omitted, and
//     defaults ProvisionedNodeGroupClaim.spec.replicas to 1.
package webhook

import (
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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	joingcp "github.com/appmana/cloud-provisioning/controller/pkg/join/gcp"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	infraGroup            = "infrastructure.cluster.x-k8s.io"
	capiGroup             = "cluster.x-k8s.io"
	cloudProvisioningGroup = "cloud-provisioning.appmana.com"
	managedByAnnotation   = "cluster.x-k8s.io/managed-by"
)

// PatchOp is a single RFC 6902 JSON Patch operation.
type PatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

// Mutator handles AdmissionReview requests and mutates incoming CAPI and
// cloud-provisioning objects.
type Mutator struct {
	Reader             client.Reader
	ManagedNamespace   string
	AWSProvider        joinaws.Provider
	AWSIdentityName    string
	GCPNetworkTag      string
	APIVIP             string
	APIServerPort      string
}

// ServeHTTP serves the /mutate admission webhook endpoint.
func (m *Mutator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "unable to read body", http.StatusBadRequest)
		return
	}
	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
		http.Error(w, "invalid AdmissionReview", http.StatusBadRequest)
		return
	}

	resp := m.Admit(r.Context(), review.Request)
	resp.UID = review.Request.UID
	outReview := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "admission.k8s.io/v1",
			Kind:       "AdmissionReview",
		},
		Response: resp,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(outReview)
}

// Admit computes the RFC 6902 JSON Patch for an AdmissionRequest.
func (m *Mutator) Admit(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req == nil || len(req.Object.Raw) == 0 {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	var orig map[string]any
	if err := json.Unmarshal(req.Object.Raw, &orig); err != nil {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	var mod map[string]any
	if err := json.Unmarshal(req.Object.Raw, &mod); err != nil {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}

	kind := req.Kind.Kind
	if kind == "" {
		if k, ok := mod["kind"].(string); ok {
			kind = k
		}
	}
	ns := req.Namespace
	if ns == "" {
		if meta, ok := mod["metadata"].(map[string]any); ok {
			ns, _ = meta["namespace"].(string)
		}
	}

	m.mutateObject(ctx, kind, ns, mod)

	ops := createMapJSONPatch(orig, mod, "")
	if len(ops) == 0 {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	patchBytes, err := json.Marshal(ops)
	if err != nil {
		return &admissionv1.AdmissionResponse{Allowed: true}
	}
	pt := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		Allowed:   true,
		Patch:     patchBytes,
		PatchType: &pt,
	}
}

func (m *Mutator) mutateObject(ctx context.Context, kind, namespace string, obj map[string]any) {
	switch kind {
	case "AWSMachineTemplate":
		spec := ensureNestedMap(obj, "spec", "template", "spec")
		arch := nestedLabel(obj, "cloud-provisioning.appmana.com/arch", "spec", "template", "metadata", "labels")
		if arch == "" {
			arch = nestedLabel(obj, "cloud-provisioning.appmana.com/arch", "metadata", "labels")
		}
		_ = joinaws.PrepareAWSMachineSpec(spec, arch, m.AWSProvider.ConfigDefaults(ctx))
	case "AWSMachine":
		spec := ensureNestedMap(obj, "spec")
		arch := nestedLabel(obj, "cloud-provisioning.appmana.com/arch", "metadata", "labels")
		_ = joinaws.PrepareAWSMachineSpec(spec, arch, m.AWSProvider.ConfigDefaults(ctx))
	case "GCPMachineTemplate":
		spec := ensureNestedMap(obj, "spec", "template", "spec")
		arch := nestedLabel(obj, "cloud-provisioning.appmana.com/arch", "spec", "template", "metadata", "labels")
		if arch == "" {
			arch = nestedLabel(obj, "cloud-provisioning.appmana.com/arch", "metadata", "labels")
		}
		_ = joingcp.PrepareGCPMachineSpec(spec, arch, m.GCPNetworkTag)
	case "GCPMachine":
		spec := ensureNestedMap(obj, "spec")
		arch := nestedLabel(obj, "cloud-provisioning.appmana.com/arch", "metadata", "labels")
		_ = joingcp.PrepareGCPMachineSpec(spec, arch, m.GCPNetworkTag)
	case "AzureMachineTemplate":
		spec := ensureNestedMap(obj, "spec", "template", "spec")
		if _, found := spec["allocatePublicIP"]; !found {
			spec["allocatePublicIP"] = true
		}
	case "AzureMachine":
		spec := ensureNestedMap(obj, "spec")
		if _, found := spec["allocatePublicIP"]; !found {
			spec["allocatePublicIP"] = true
		}
	case "Cluster":
		if m.ManagedNamespace == "" || namespace == m.ManagedNamespace {
			m.ensureManagedByExternal(obj)
			m.defaultClusterSpec(obj)
		}
	case "AWSCluster", "GCPCluster", "AzureCluster":
		if m.ManagedNamespace == "" || namespace == m.ManagedNamespace {
			m.ensureManagedByExternal(obj)
			m.defaultControlPlaneEndpoint(ensureNestedMap(obj, "spec"))
			if kind == "AWSCluster" {
				m.defaultAWSClusterIdentity(ctx, ensureNestedMap(obj, "spec"))
			}
		}
	case "ProvisionedNodeClaim":
		spec := ensureNestedMap(obj, "spec")
		m.defaultClaimSpec(ctx, namespace, spec)
	case "ProvisionedNodeGroupClaim":
		spec := ensureNestedMap(obj, "spec")
		if _, found := spec["replicas"]; !found {
			spec["replicas"] = float64(1)
		}
		tmplSpec := ensureNestedMap(obj, "spec", "template", "spec")
		m.defaultClaimSpec(ctx, namespace, tmplSpec)
	}
}

func (m *Mutator) ensureManagedByExternal(obj map[string]any) {
	anns := ensureNestedMap(obj, "metadata", "annotations")
	if _, found := anns[managedByAnnotation]; !found {
		anns[managedByAnnotation] = "external"
	}
}

func (m *Mutator) defaultClusterSpec(obj map[string]any) {
	spec := ensureNestedMap(obj, "spec")
	m.defaultControlPlaneEndpoint(spec)
	infraRef, _ := spec["infrastructureRef"].(map[string]any)
	if infraRef == nil {
		return
	}
	if grp, _ := infraRef["apiGroup"].(string); strings.TrimSpace(grp) == "" {
		infraRef["apiGroup"] = infraGroup
	}
	if name, _ := infraRef["name"].(string); strings.TrimSpace(name) == "" {
		if meta, ok := obj["metadata"].(map[string]any); ok {
			if objName, _ := meta["name"].(string); objName != "" {
				infraRef["name"] = objName
			}
		}
	}
}

func (m *Mutator) defaultControlPlaneEndpoint(spec map[string]any) {
	if strings.TrimSpace(m.APIVIP) == "" {
		return
	}
	ep, _ := spec["controlPlaneEndpoint"].(map[string]any)
	if ep == nil {
		ep = map[string]any{}
	}
	host, _ := ep["host"].(string)
	if strings.TrimSpace(host) == "" {
		ep["host"] = m.APIVIP
	}
	if _, hasPort := ep["port"]; !hasPort {
		portNum := 6443
		if p, err := strconv.Atoi(strings.TrimSpace(m.APIServerPort)); err == nil && p > 0 {
			portNum = p
		}
		ep["port"] = float64(portNum)
	}
	spec["controlPlaneEndpoint"] = ep
}

func (m *Mutator) defaultAWSClusterIdentity(ctx context.Context, spec map[string]any) {
	if _, hasIdentity := spec["identityRef"]; hasIdentity {
		return
	}
	idName := strings.TrimSpace(m.AWSIdentityName)
	if idName == "" {
		return
	}
	kind := "AWSClusterStaticIdentity"
	if m.Reader != nil {
		role := &unstructured.Unstructured{}
		role.SetGroupVersionKind(schema.GroupVersionKind{Group: infraGroup, Version: "v1beta2", Kind: "AWSClusterRoleIdentity"})
		if err := m.Reader.Get(ctx, client.ObjectKey{Name: idName}, role); err == nil {
			kind = "AWSClusterRoleIdentity"
		}
	}
	spec["identityRef"] = map[string]any{
		"kind": kind,
		"name": idName,
	}
}

func (m *Mutator) defaultClaimSpec(ctx context.Context, namespace string, spec map[string]any) {
	if _, found := spec["internetFacing"]; !found {
		spec["internetFacing"] = true
	}
	infraRef, _ := spec["infrastructureRef"].(map[string]any)
	if infraRef != nil {
		if grp, _ := infraRef["apiGroup"].(string); strings.TrimSpace(grp) == "" {
			infraRef["apiGroup"] = infraGroup
		}
		if kind, _ := infraRef["kind"].(string); kind != "" && !strings.HasSuffix(kind, "Template") {
			infraRef["kind"] = kind + "Template"
		}
	}
	if clusterName, _ := spec["clusterName"].(string); strings.TrimSpace(clusterName) == "" && m.Reader != nil && namespace != "" {
		if resolved := m.resolveDefaultClusterName(ctx, namespace, infraRef); resolved != "" {
			spec["clusterName"] = resolved
		}
	}
}

func (m *Mutator) resolveDefaultClusterName(ctx context.Context, namespace string, infraRef map[string]any) string {
	clusters := &unstructured.UnstructuredList{}
	clusters.SetGroupVersionKind(schema.GroupVersionKind{Group: capiGroup, Version: "v1beta2", Kind: "ClusterList"})
	if err := m.Reader.List(ctx, clusters, client.InNamespace(namespace)); err != nil || len(clusters.Items) == 0 {
		return ""
	}
	if len(clusters.Items) == 1 {
		return clusters.Items[0].GetName()
	}
	tmplKind, _ := infraRef["kind"].(string)
	wantClusterKind := strings.TrimSuffix(strings.TrimSuffix(tmplKind, "Template"), "Machine") + "Cluster"
	var matches []string
	for _, c := range clusters.Items {
		ck, _, _ := unstructured.NestedString(c.Object, "spec", "infrastructureRef", "kind")
		if ck == wantClusterKind {
			matches = append(matches, c.GetName())
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

func ensureNestedMap(obj map[string]any, fields ...string) map[string]any {
	cur := obj
	for _, f := range fields {
		next, ok := cur[f].(map[string]any)
		if !ok || next == nil {
			next = map[string]any{}
			cur[f] = next
		}
		cur = next
	}
	return cur
}

func nestedLabel(obj map[string]any, key string, path ...string) string {
	cur := obj
	for _, f := range path {
		next, ok := cur[f].(map[string]any)
		if !ok {
			return ""
		}
		cur = next
	}
	v, _ := cur[key].(string)
	return v
}

func createMapJSONPatch(orig, mod map[string]any, prefix string) []PatchOp {
	var ops []PatchOp
	modKeys := make([]string, 0, len(mod))
	for k := range mod {
		modKeys = append(modKeys, k)
	}
	sort.Strings(modKeys)
	for _, k := range modKeys {
		path := prefix + "/" + escapeJSONPointer(k)
		modVal := mod[k]
		origVal, exists := orig[k]
		if !exists {
			ops = append(ops, PatchOp{Op: "add", Path: path, Value: modVal})
			continue
		}
		origSub, origIsMap := origVal.(map[string]any)
		modSub, modIsMap := modVal.(map[string]any)
		if origIsMap && modIsMap {
			ops = append(ops, createMapJSONPatch(origSub, modSub, path)...)
			continue
		}
		if !reflect.DeepEqual(origVal, modVal) {
			ops = append(ops, PatchOp{Op: "replace", Path: path, Value: modVal})
		}
	}
	origKeys := make([]string, 0, len(orig))
	for k := range orig {
		if _, exists := mod[k]; !exists {
			origKeys = append(origKeys, k)
		}
	}
	sort.Strings(origKeys)
	for _, k := range origKeys {
		ops = append(ops, PatchOp{Op: "remove", Path: prefix + "/" + escapeJSONPointer(k)})
	}
	return ops
}

func escapeJSONPointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// Server runs the HTTPS mutating admission webhook server as a controller-runtime Runnable.
type Server struct {
	BindAddress string
	CertDir     string
	Mutator     *Mutator
}

// NeedLeaderElection returns false so every webhook pod can serve admission requests.
func (*Server) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	if strings.TrimSpace(s.BindAddress) == "" || strings.TrimSpace(s.BindAddress) == "0" {
		return nil
	}
	tlsCfg, err := s.tlsConfig()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", s.BindAddress)
	if err != nil {
		return fmt.Errorf("listening on webhook address %s: %w", s.BindAddress, err)
	}
	defer listener.Close()

	mux := http.NewServeMux()
	mux.Handle("/mutate", s.Mutator)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Handler:           mux,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ServeTLS(listener, "", "")
	}()
	select {
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	}
}

func (s *Server) tlsConfig() (*tls.Config, error) {
	certPath := filepath.Join(s.CertDir, "tls.crt")
	keyPath := filepath.Join(s.CertDir, "tls.key")
	if _, err := os.Stat(certPath); err == nil {
		return &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				cert, err := tls.LoadX509KeyPair(certPath, keyPath)
				if err != nil {
					return nil, err
				}
				return &cert, nil
			},
		}, nil
	}
	// Fallback self-signed certificate when cert files are not mounted on disk.
	cert, err := generateSelfSignedCert()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}, nil
}

func generateSelfSignedCert() (tls.Certificate, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "cloud-provisioning-webhook"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "cloud-provisioning-endpoint-controller-webhook.cloud-provisioning.svc"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
	return tls.X509KeyPair(certPEM, keyPEM)
}

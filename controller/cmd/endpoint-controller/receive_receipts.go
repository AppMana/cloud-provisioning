package main

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const receiveReceiptAudience = "cloud-provisioning-receipts"

// Image IDs are immutable release inputs indexed by node architecture. They
// are never learned from an agent's claims or its current pod status.
type receiveReceiptPolicy struct {
	BindAddress string            `json:"bindAddress"`
	CertFile    string            `json:"certFile"`
	KeyFile     string            `json:"keyFile"`
	ImageIDs    map[string]string `json:"imageIDs"`
	Endpoint    string            `json:"endpoint,omitempty"`
	TrustSecret string            `json:"trustSecret,omitempty"`
}

func (p receiveReceiptPolicy) validate() error {
	if p.Endpoint != "" || p.TrustSecret != "" {
		u, err := url.Parse(p.Endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || p.TrustSecret == "" {
			return fmt.Errorf("receipt agents require a direct HTTPS endpoint and CA Secret")
		}
	}
	_, port, err := net.SplitHostPort(p.BindAddress)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 1 || number > 65535 || p.CertFile == "" || p.KeyFile == "" || len(p.ImageIDs) == 0 {
		return fmt.Errorf("receipt serving requires an explicit address, TLS files and immutable agent image IDs")
	}
	for arch, id := range p.ImageIDs {
		if arch != "amd64" && arch != "arm64" {
			return fmt.Errorf("receipt release has unsupported architecture")
		}
		digest := strings.TrimPrefix(id, "sha256:")
		if name, suffix, found := strings.Cut(id, "@sha256:"); found {
			if name == "" || strings.Contains(name, "@") {
				return fmt.Errorf("receipt image ID has an invalid image name")
			}
			digest = suffix
		}
		raw, err := hex.DecodeString(digest)
		if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != digest || strings.ContainsAny(id, " \t\r\n") || digest == id {
			return fmt.Errorf("receipt release requires immutable runtime image IDs")
		}
	}
	return nil
}

// Validate the configured release before creating journal or agent resources.
func validateReceiptAgentRelease(policy *receivePreparationPolicy, image, cloudImage string) error {
	if policy == nil || policy.Journal == nil || policy.Journal.Receipts == nil {
		return nil
	}
	if cloudImage != "" {
		return fmt.Errorf("receipt advancement requires image-entrypoint cloud agents")
	}
	for _, id := range policy.Journal.Receipts.ImageIDs {
		if err := handover.ValidateReceiptImagePolicy(image, id); err != nil {
			return err
		}
	}
	return nil
}

func (r *meshReconciler) configureReceiptAgent(daemon *appsv1.DaemonSet) {
	if r.receivePreparation == nil || r.receivePreparation.Journal == nil || r.receivePreparation.Journal.Receipts == nil {
		return
	}
	p := r.receivePreparation.Journal.Receipts
	if p.Endpoint == "" {
		return
	}
	// These agents use host networking, but the receipt service is addressed
	// through cluster DNS. The ordinary ClusterFirst fallback uses host DNS.
	daemon.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	const directory = "/var/run/cloud-provisioning/receipts"
	agent := &daemon.Spec.Template.Spec.Containers[0]
	agent.Args = append(agent.Args, "--receipt-url="+p.Endpoint, "--receipt-ca-file="+directory+"/ca.crt", "--receipt-token-file="+directory+"/token", "--receive-mesh-name="+r.secretName)
	agent.Env = append(agent.Env,
		corev1.EnvVar{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		corev1.EnvVar{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}})
	agent.VolumeMounts = append(agent.VolumeMounts, corev1.VolumeMount{Name: "receive-credentials", MountPath: directory, ReadOnly: true})
	expiry := int64(600)
	daemon.Spec.Template.Spec.Volumes = append(daemon.Spec.Template.Spec.Volumes, corev1.Volume{Name: "receive-credentials", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
		{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: receiveReceiptAudience, ExpirationSeconds: &expiry, Path: "token"}},
		{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: p.TrustSecret}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
	}}}})
}

// resolveReceiveReceipts admits the target from the controller-owned inventory
// and current machine/mesh identities before allowing any journal mutation.
func (r *meshReconciler) resolveReceiveReceipts(ctx context.Context, api kubernetes.Interface, policy receiveReceiptPolicy, machineUID string) (handover.Store, map[string]handover.PodReceiptAuthenticator, error) {
	fail := func(err error) (handover.Store, map[string]handover.PodReceiptAuthenticator, error) {
		return handover.Store{}, nil, err
	}
	if api == nil || r.receivePreparation == nil || r.receivePreparation.Journal == nil || r.dialerCloudImage != "" {
		return fail(fmt.Errorf("receipt advancement requires journal policy and image-entrypoint agents"))
	}
	mesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		return fail(err)
	}
	if mesh.UID == "" || mesh.DeletionTimestamp != nil {
		return fail(fmt.Errorf("receipt mesh is not live"))
	}
	store := r.receiveJournal
	inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, r.secretNamespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return fail(err)
	}
	if !slices.Contains(inventory.MachineUIDs, machineUID) {
		return fail(fmt.Errorf("receipt machine has no reserved journal"))
	}
	store.Name = tunnel.ReceiveTransitionName(machineUID)
	snapshot, err := store.Load(ctx)
	if err != nil {
		return fail(err)
	}
	metadata := snapshot.Metadata()
	if err := inventory.CheckJournal(machineUID, string(metadata.UID)); err != nil {
		return fail(err)
	}
	if metadata.DeletionTimestamp != nil || metadata.Namespace != r.secretNamespace {
		return fail(fmt.Errorf("receipt journal scope changed"))
	}
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	if err != nil {
		return fail(err)
	}
	if plan.MachineUID != machineUID || plan.MeshUID != string(mesh.UID) || plan.MeshName != mesh.Name || plan.Namespace != r.secretNamespace {
		return fail(fmt.Errorf("receipt plan differs from reserved scope"))
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: plan.MachineName}, machine); err != nil {
		return fail(err)
	}
	if string(machine.GetUID()) != machineUID || machine.GetDeletionTimestamp() != nil {
		return fail(fmt.Errorf("receipt machine incarnation changed"))
	}
	if len(plan.CloudAddresses) == 0 || !slices.Equal(plan.CloudAddresses, receiveMachineAddresses(machine)) || strings.TrimSpace(string(mesh.Data[tunnel.PeerPublicKeyPrefix+plan.MachineName])) != plan.RootPublicKey {
		return fail(fmt.Errorf("receipt cloud allocation or root identity changed"))
	}
	if err := attachment.CheckPeerPublication(ctx, r.reader, machine); err != nil {
		return fail(err)
	}
	adoption := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: plan.AdoptionName}, adoption); err != nil {
		return fail(err)
	}
	if string(adoption.UID) != plan.AdoptionUID || adoption.DeletionTimestamp != nil {
		return fail(fmt.Errorf("receipt adoption incarnation changed"))
	}
	sa, err := api.CoreV1().ServiceAccounts(r.secretNamespace).Get(ctx, r.dialerServiceAccount, metav1.GetOptions{})
	if err != nil {
		return fail(err)
	}
	if sa.UID == "" || sa.DeletionTimestamp != nil {
		return fail(fmt.Errorf("receipt agent account is not live"))
	}
	policies := map[string]handover.PodReceiptAuthenticator{}
	nodes := map[string]string{plan.CloudNodeName: plan.CloudNodeUID}
	for public, site := range plan.Sites {
		if len(site.RouteSources) == 0 || !slices.Equal(site.RouteSources, tunnel.ReceiveSiteRouteSources(mesh.Data, site.NodeName)) {
			return fail(fmt.Errorf("receipt site route sources changed"))
		}
		if strings.TrimSpace(string(mesh.Data[tunnel.NodePublicKeyPrefix+site.NodeName])) != public || site.Legacy == nil || site.Legacy.Tunnel == nil {
			return fail(fmt.Errorf("receipt site source identity is missing or changed"))
		}
		if err := tunnel.ValidateReceiveSiteTunnelAllocation(*site.Legacy.Tunnel, mesh.Data, site.NodeName); err != nil {
			return fail(err)
		}
		nodes[site.NodeName] = site.NodeUID
	}
	for name, uid := range nodes {
		node, err := api.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fail(err)
		}
		if string(node.UID) != uid || node.DeletionTimestamp != nil || node.Status.NodeInfo.OperatingSystem != "linux" {
			return fail(fmt.Errorf("receipt participant identity or platform changed"))
		}
		imageID := policy.ImageIDs[node.Status.NodeInfo.Architecture]
		if imageID == "" {
			return fail(fmt.Errorf("receipt participant architecture has no admitted release"))
		}
		daemonName := r.dialerDaemonSetName
		if uid == plan.CloudNodeUID {
			daemonName = r.dialerCloudDaemonSetName
			provider, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
			refUID, _, _ := unstructured.NestedString(machine.Object, "status", "nodeRef", "uid")
			if provider == "" || node.Spec.ProviderID != provider || (refUID != "" && refUID != uid) {
				return fail(fmt.Errorf("receipt cloud machine placement changed"))
			}
		}
		daemon, err := api.AppsV1().DaemonSets(r.secretNamespace).Get(ctx, daemonName, metav1.GetOptions{})
		if err != nil {
			return fail(err)
		}
		if daemon.UID == "" || daemon.DeletionTimestamp != nil {
			return fail(fmt.Errorf("receipt agent daemon is not live"))
		}
		policies[uid] = handover.PodReceiptAuthenticator{API: api, Audience: receiveReceiptAudience, Namespace: r.secretNamespace, ServiceAccountName: sa.Name, ServiceAccountUID: string(sa.UID), DaemonSetName: daemon.Name, DaemonSetUID: string(daemon.UID), ContainerName: "dialer", Image: r.dialerImage, ImageID: imageID}
	}
	currentMesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKeyFromObject(mesh), currentMesh); err != nil {
		return fail(err)
	}
	_, currentIndex, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, r.secretNamespace, mesh.Name, string(mesh.UID))
	if err != nil {
		return fail(err)
	}
	if currentMesh.UID != mesh.UID || currentMesh.ResourceVersion != mesh.ResourceVersion || currentIndex.UID != index.UID || currentIndex.ResourceVersion != index.ResourceVersion {
		return fail(fmt.Errorf("receipt admission changed during verification"))
	}
	store.ExpectedUID = string(metadata.UID)
	return store, policies, nil
}

type receiveReceiptServer struct {
	policy  receiveReceiptPolicy
	handler *handover.ReceiptHandler
}

func (*receiveReceiptServer) NeedLeaderElection() bool { return true }

func (s *receiveReceiptServer) Start(ctx context.Context) error {
	certificate, err := tls.LoadX509KeyPair(s.policy.CertFile, s.policy.KeyFile)
	if err != nil {
		return fmt.Errorf("load receipt serving identity: %w", err)
	}
	listener, err := net.Listen("tcp", s.policy.BindAddress)
	if err != nil {
		return err
	}
	return s.serve(ctx, listener, certificate)
}

func (s *receiveReceiptServer) serve(ctx context.Context, listener net.Listener, certificate tls.Certificate) error {
	defer listener.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mux := http.NewServeMux()
	mux.Handle("/v1/receipts", s.handler)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32768, TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}, BaseContext: func(net.Listener) context.Context { return ctx }}
	collectorDone := make(chan struct{})
	go func() { s.handler.Run(ctx); close(collectorDone) }()
	result := make(chan error, 1)
	go func() { result <- server.ServeTLS(listener, "", "") }()
	var err error
	select {
	case err = <-result:
	case <-ctx.Done():
		err = nil
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if server.Shutdown(shutdown) != nil {
		_ = server.Close()
	}
	<-collectorDone
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

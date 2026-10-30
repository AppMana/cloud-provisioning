package check

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIngressObjectsAndSingleENIRollingUpdateStrategy(t *testing.T) {
	deploy, gw := IngressObjects("ingress-gateway", "http://site-backend.site-apps.svc.cluster.local:8080", 1)
	if deploy.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || deploy.Spec.Strategy.RollingUpdate == nil {
		t.Fatalf("expected RollingUpdate strategy, got %+v", deploy.Spec.Strategy)
	}
	if got := deploy.Spec.Strategy.RollingUpdate.MaxSurge.IntValue(); got != 0 {
		t.Fatalf("RollingUpdate maxSurge = %d, want 0 on single-ENI hostPort 80/443 worker", got)
	}
	if got := deploy.Spec.Strategy.RollingUpdate.MaxUnavailable.IntValue(); got != 1 {
		t.Fatalf("RollingUpdate maxUnavailable = %d, want 1", got)
	}
	if got := deploy.Spec.Template.Spec.NodeSelector[CloudWorkerRoleLabel]; got != CloudWorkerRoleValue {
		t.Fatalf("nodeSelector[%s] = %q, want %q", CloudWorkerRoleLabel, got, CloudWorkerRoleValue)
	}
	toleratesInternetFacing := false
	for _, tol := range deploy.Spec.Template.Spec.Tolerations {
		if tol.Key == InternetFacingTaintKey && tol.Value == InternetFacingTaintValue && tol.Effect == corev1.TaintEffectNoSchedule {
			toleratesInternetFacing = true
		}
	}
	if !toleratesInternetFacing {
		t.Fatalf("ingress proxy deployment missing %s=%s:NoSchedule toleration", InternetFacingTaintKey, InternetFacingTaintValue)
	}
	ports := map[int32]int32{}
	for _, p := range deploy.Spec.Template.Spec.Containers[0].Ports {
		ports[p.ContainerPort] = p.HostPort
	}
	if ports[80] != 80 || ports[443] != 443 {
		t.Fatalf("expected hostPort 80 and 443, got %v", ports)
	}
	if gw.GetKind() != "Gateway" || gw.GetName() != "public-gateway" {
		t.Fatalf("unexpected Gateway object: %+v", gw)
	}
	ann := gw.GetAnnotations()
	if ann["external-dns.alpha.kubernetes.io/hostname"] != "example.test,*.example.test" {
		t.Fatalf("unexpected Gateway annotations: %v", ann)
	}

	sitePod := SiteWorkloadObject("site-backend", "site-apps")
	for _, tol := range sitePod.Spec.Tolerations {
		if tol.Key == InternetFacingTaintKey {
			t.Fatalf("ordinary site workload pod must not tolerate %s", InternetFacingTaintKey)
		}
	}
}

func TestValidateIngressPlacementAndNodeExternalIP(t *testing.T) {
	cloudNode := "ip-10-200-0-12.ap-east-1.compute.internal"
	siteNode := "w1"
	if err := ValidateIngressPlacement(cloudNode, cloudNode, siteNode); err != nil {
		t.Fatalf("expected valid placement, got %v", err)
	}
	if err := ValidateIngressPlacement("", cloudNode, siteNode); err == nil {
		t.Fatal("expected error for empty cloudNode")
	}
	if err := ValidateIngressPlacement(cloudNode, siteNode, siteNode); err == nil {
		t.Fatal("expected error when ingress proxy schedules on site node")
	}
	if err := ValidateIngressPlacement(cloudNode, cloudNode, cloudNode); err == nil {
		t.Fatal("expected error when untolerated site pod schedules on internet-facing cloud worker")
	}

	validNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   cloudNode,
			Labels: map[string]string{CloudWorkerRoleLabel: CloudWorkerRoleValue},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{
				Key:    InternetFacingTaintKey,
				Value:  InternetFacingTaintValue,
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.200.0.12"},
				{Type: corev1.NodeExternalIP, Address: "18.162.45.99"},
			},
		},
	}
	ip, err := ExtractNodeExternalIP(validNode)
	if err != nil || ip != "18.162.45.99" {
		t.Fatalf("ExtractNodeExternalIP = (%q, %v), want (18.162.45.99, nil)", ip, err)
	}

	noTaint := validNode.DeepCopy()
	noTaint.Spec.Taints = nil
	if _, err := ExtractNodeExternalIP(noTaint); err == nil {
		t.Fatal("expected error for cloud worker missing internet-facing NoSchedule taint")
	}

	noExt := validNode.DeepCopy()
	noExt.Status.Addresses = noExt.Status.Addresses[:1]
	if _, err := ExtractNodeExternalIP(noExt); err == nil {
		t.Fatal("expected error for cloud worker missing NodeExternalIP")
	}
}

func TestExerciseIngressDataPlaneEndToEnd(t *testing.T) {
	siteUpstream := httptest.NewServer(NewSiteUpstreamHandler("w1"))
	defer siteUpstream.Close()

	var gen atomic.Int32
	gen.Store(1)
	proxyHandler := NewCloudIngressProxyHandler(siteUpstream.URL, siteUpstream.Client(), func() int {
		return int(gen.Load())
	})

	httpProxy := httptest.NewServer(proxyHandler)
	defer httpProxy.Close()

	cert, roots, err := SelfSignedIngressTLS("127.0.0.1", "auth.example.test")
	if err != nil {
		t.Fatalf("SelfSignedIngressTLS: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	tlsListener := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	httpsServer := &http.Server{Handler: proxyHandler}
	go func() { _ = httpsServer.Serve(tlsListener) }()
	defer httpsServer.Close()
	httpsURL := "https://" + ln.Addr().String()

	report := &IngressReport{
		CloudNode:         "ip-10-200-0-12.ap-east-1.compute.internal",
		ExternalIP:        "18.162.45.99",
		GatewayTarget:     "18.162.45.99",
		ProxyNode:         "ip-10-200-0-12.ap-east-1.compute.internal",
		SiteBackendNode:   "w1",
		PlacementIsolated: true,
	}
	err = ExerciseIngressDataPlane(
		context.Background(),
		httpProxy.URL,
		httpsURL,
		roots,
		func() error {
			gen.Store(2)
			return nil
		},
		func(ctx context.Context) error {
			return nil
		},
		report,
	)
	if err != nil {
		t.Fatalf("ExerciseIngressDataPlane failed: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected report.OK() == true, got %+v", report)
	}
	if report.LargeBodyBytes < IngressMinTransferBytes {
		t.Fatalf("LargeBodyBytes = %d, want >= %d", report.LargeBodyBytes, IngressMinTransferBytes)
	}
	if report.SSEChunksReceived < 3 {
		t.Fatalf("SSEChunksReceived = %d, want >= 3", report.SSEChunksReceived)
	}

	// Verify OIDC control-plane hairpin failure during rollout fails closed.
	badReport := *report
	badReport.OIDCHairpinSafe = false
	err = ExerciseIngressDataPlane(
		context.Background(),
		httpProxy.URL,
		httpsURL,
		roots,
		func() error { return nil },
		func(ctx context.Context) error { return errors.New("apiserver OIDC hairpin unreachable") },
		&badReport,
	)
	if err == nil {
		t.Fatal("expected error when OIDC control-plane hairpin check fails during ingress rollout")
	}
}

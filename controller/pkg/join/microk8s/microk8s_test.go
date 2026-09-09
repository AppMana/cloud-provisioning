package microk8s

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Shape and version match native add-node and Node observations from snap 9063.
// Token bytes are synthetic; no live join credential belongs in this fixture.
func TestNativeCredentialValidation(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	original := Config{JoinURL: "10.10.0.10:25000/0123456789abcdef0123456789abcdef/0123456789ab", Revision: "9063", Version: "v1.34.9", ExpiresAt: now.Add(time.Hour)}
	for _, tc := range []struct {
		name    string
		change  func(*Config)
		version string
		wantErr bool
	}{
		{"observed native shape", func(*Config) {}, "v1.34.9", false},
		{"expired token", func(c *Config) { c.ExpiresAt = now }, "v1.34.9", true},
		{"insufficient bootstrap window", func(c *Config) { c.ExpiresAt = now.Add(5 * time.Minute) }, "v1.34.9", true},
		{"API token is not cluster agent token", func(c *Config) { c.JoinURL = "10.10.0.10:25000/abcdef.0123456789abcdef" }, "v1.34.9", true},
		{"wrong API port", func(c *Config) { c.JoinURL = strings.Replace(c.JoinURL, ":25000/", ":16443/", 1) }, "v1.34.9", true},
		{"unpinned revision", func(c *Config) { c.Revision = "latest" }, "v1.34.9", true},
		{"version mismatch", func(*Config) {}, "v1.34.8", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := original
			tc.change(&cfg)
			raw, _ := json.Marshal(cfg)
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "microk8s-provider-config", Namespace: "test"}, Data: map[string][]byte{"config.json": raw}},
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: tc.version}}},
			).Build()
			p := Provider{Reader: reader, Namespace: "test", Now: func() time.Time { return now }}
			got, err := p.JoinValues(context.Background())
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v wantError=%v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "0123456789") {
				t.Fatal("error exposes native credential")
			}
			if err == nil && (got["apiEndpoint"] != "10.10.0.10:16443" || got["microk8sRevision"] != "9063" || got["microk8sJoinURL"] != cfg.JoinURL) {
				t.Fatal("native credential or API endpoint changed")
			}
		})
	}
}

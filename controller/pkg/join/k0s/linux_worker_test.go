package k0s

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"strings"
	"testing"
)

func TestBakedLinuxWorkerAdmission(t *testing.T) {
	const base = "v1.36.4+k0s.1"
	for _, tc := range []struct {
		name, version, amd, arm string
		valid                   bool
	}{
		{"absent", "absent", "absent", "absent", true},
		{"amd64", base + ".stream.4", strings.Repeat("A", 64), "absent", true},
		{"arm64", base + ".stream.4", "absent", strings.Repeat("b", 64), true},
		{"no digest", base, "absent", "absent", false},
		{"no version", "absent", strings.Repeat("a", 64), "absent", false},
		{"empty digest", base, "", "absent", false},
		{"bad digest", base, strings.Repeat("g", 64), "absent", false},
		{"wrong release", "v1.36.4+k0s.10.stream.4", strings.Repeat("a", 64), "absent", false},
		{"unsafe version", base + ".';true", strings.Repeat("a", 64), "absent", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string][]byte{}
			for k, v := range map[string]string{"worker-version": tc.version, "worker-sha256-amd64": tc.amd, "worker-sha256-arm64": tc.arm} {
				if v != "absent" {
					data[k] = []byte(v)
				}
			}
			cfg := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "test"}, Data: data}
			p := Provider{Client: fake.NewSimpleClientset(cfg), ConfigName: "config", ConfigNamespace: "test"}
			got, err := p.linuxWorkerValues(context.Background(), base)
			if (err == nil) != tc.valid {
				t.Fatalf("values=%v error=%v", got, err)
			}
			if tc.valid && (got["k0sWorkerVersion"] != string(data["worker-version"]) || got["k0sWorkerSHA256Amd64"] != strings.ToLower(string(data["worker-sha256-amd64"]))) {
				t.Fatal("worker identity changed")
			}
		})
	}
}

func TestBakedLinuxWorkerJoinValues(t *testing.T) {
	const base = "v1.36.4+k0s.1"
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "admitted", false: "rejected before token"}[valid], func(t *testing.T) {
			digest := strings.Repeat("a", 64)
			if !valid {
				digest = "invalid"
			}
			api := fake.NewSimpleClientset(
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: metav1.NamespaceSystem}, Data: map[string]string{"ca.crt": "test-ca"}},
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cp"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.36.4+k0s"}}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "test"}, Data: map[string][]byte{"worker-version": []byte(base + ".custom.1"), "worker-sha256-amd64": []byte(digest)}},
			)
			p := Provider{Client: api, ConfigName: "config", ConfigNamespace: "test", APIAddress: "https://192.0.2.1:6443", GitHubReleasesAPI: fakeGitHubReleasesServer(t, base)}
			got, err := p.JoinValues(context.Background())
			if (err == nil) != valid {
				t.Fatalf("join error=%v", err)
			}
			tokens, err := api.CoreV1().Secrets(metav1.NamespaceSystem).List(context.Background(), metav1.ListOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !valid {
				if len(tokens.Items) != 0 {
					t.Fatal("invalid worker configuration minted a bootstrap credential")
				}
				return
			}
			if len(tokens.Items) != 1 || got["k0sVersion"] != base || got["k0sWorkerVersion"] != base+".custom.1" || got["k0sWorkerSHA256Amd64"] != digest {
				t.Fatal("join values lost the base release or pinned worker identity")
			}
		})
	}
}

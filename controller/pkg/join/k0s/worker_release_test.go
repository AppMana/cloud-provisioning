package k0s

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimefake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExplicitWorkerReleaseForPatchedControllers(t *testing.T) {
	const base = "v1.36.4+k0s.1"
	for _, tc := range []struct {
		name, configured string
		controllers      []string
		want             string
	}{
		{"patched", base, []string{base + ".egress.2", base + ".egress.2"}, base},
		{"stock", base, []string{base}, base},
		{"stock without override", "absent", []string{base}, base},
		{"stock without config Secret", "missing", []string{base}, base},
		{"no opt in", "absent", []string{base + ".egress.2"}, ""},
		{"different upstream build", "v1.36.4+k0s.0", []string{base + ".egress.2"}, ""},
		{"different patch release", "v1.36.3+k0s.1", []string{base + ".egress.2"}, ""},
		{"prefix is not release", base, []string{"v1.36.4+k0s.10.egress.2"}, ""},
		{"mixed patched builds", base, []string{base + ".egress.1", base + ".egress.2"}, ""},
		{"mixed stock and patched", base, []string{base, base + ".egress.2"}, ""},
		{"invalid controller suffix", base, []string{base + ".;bad"}, ""},
		{"worker must be upstream release", base + ".winfix.1", []string{base + ".egress.2"}, ""},
		{"empty explicit release", "", []string{base}, ""},
		{"no observed controllers", base, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			gvk := schema.GroupVersionKind{Group: "autopilot.k0sproject.io", Version: "v1beta2", Kind: "ControlNode"}
			scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
			scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ControlNodeList"), &unstructured.UnstructuredList{})
			var objects []client.Object
			for i, version := range tc.controllers {
				obj := &unstructured.Unstructured{}
				obj.SetGroupVersionKind(gvk)
				obj.SetName("cp-" + strings.Repeat("a", i+1))
				if err := unstructured.SetNestedField(obj.Object, version, "status", "k0sVersion"); err != nil {
					t.Fatal(err)
				}
				objects = append(objects, obj)
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "k0s-provider-config"}, Data: map[string][]byte{}}
			if tc.configured != "absent" {
				secret.Data["worker-release"] = []byte(tc.configured)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				t.Error("explicit release selection contacted GitHub")
				w.WriteHeader(http.StatusForbidden)
			}))
			defer server.Close()
			api := fake.NewSimpleClientset()
			if tc.configured != "missing" {
				api = fake.NewSimpleClientset(secret)
			}
			p := &Provider{Reader: runtimefake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), Client: api, ConfigNamespace: "test", ConfigName: secret.Name, GitHubReleasesAPI: server.URL}
			got, err := p.introspectK0sVersion(context.Background())
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted invalid worker release selection: %q", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("release=%q, error=%v; want %q", got, err, tc.want)
			}
			values := windowsValues(t)
			values["k0sVersion"] = got
			worker := WindowsBootstrap{ServiceSHA256: strings.Repeat("a", 64), WorkerVersion: base + ".winfix.1", WorkerSHA256: strings.Repeat("b", 64)}
			if _, err := worker.Render(values); err != nil {
				t.Fatalf("same-base pinned Windows build rejected: %v", err)
			}
		})
	}
}

func TestWorkerReleaseConfigurationFailureStopsDiscovery(t *testing.T) {
	api := fake.NewSimpleClientset()
	api.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "k0s-provider-config", nil)
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unreadable provider configuration fell back to GitHub")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	p := &Provider{Client: api, ConfigNamespace: "test", ConfigName: "k0s-provider-config", GitHubReleasesAPI: server.URL}
	if _, err := p.introspectK0sVersion(context.Background()); !apierrors.IsForbidden(err) {
		t.Fatalf("configuration read failure was hidden: %v", err)
	}
}

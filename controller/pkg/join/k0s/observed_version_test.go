package k0s

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimefake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ControlNode status includes the release counter omitted by kubelet versions.
func TestObservedControlNodesProvideTheExactInstalledRelease(t *testing.T) {
	for _, observed := range []struct{ file, release string }{
		{"testdata/controlnodes-v1.34.1.json", "v1.34.1+k0s.0"},
		{"testdata/controlnodes-v1.36.4.json", "v1.36.4+k0s.1"},
	} {
		t.Run(observed.release, func(t *testing.T) {
			observedControlNodes(t, observed.file, observed.release)
		})
	}
}

func observedControlNodes(t *testing.T, file, release string) {
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var list unstructured.UnstructuredList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	for _, mixed := range []bool{false, true} {
		if mixed && len(list.Items) < 2 {
			continue // one controller cannot disagree with itself
		}
		scheme := runtime.NewScheme()
		gvk := schema.GroupVersionKind{Group: "autopilot.k0sproject.io", Version: "v1beta2", Kind: "ControlNode"}
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind("ControlNodeList"), &unstructured.UnstructuredList{})
		var objects []client.Object
		for i := range list.Items {
			objects = append(objects, list.Items[i].DeepCopy())
		}
		if mixed {
			_ = unstructured.SetNestedField(objects[0].(*unstructured.Unstructured).Object, release+".other", "status", "k0sVersion")
		}
		c := runtimefake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("version introspection contacted GitHub")
			w.WriteHeader(http.StatusForbidden)
		}))
		p := &Provider{Reader: c, GitHubReleasesAPI: server.URL}
		got, err := p.introspectK0sVersion(context.Background())
		server.Close()
		if mixed {
			if err == nil {
				t.Fatal("mixed controller versions were guessed")
			}
			continue
		}
		if err != nil || got != release {
			t.Fatalf("release=%q err=%v", got, err)
		}
	}
}

package docker

import (
	"context"
	"runtime"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestInfraValues_ArchIsTheHosts(t *testing.T) {
	values, err := Provider{}.InfraValues(context.Background(), &unstructured.Unstructured{})
	if err != nil {
		t.Fatalf("InfraValues: %v", err)
	}
	if values["arch"] != runtime.GOARCH {
		t.Errorf("arch = %v, want the host's %s", values["arch"], runtime.GOARCH)
	}
}

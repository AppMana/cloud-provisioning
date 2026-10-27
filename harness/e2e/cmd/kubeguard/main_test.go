package main

import (
	"bytes"
	"strings"
	"testing"
)

// A refused write never reaches kubectl, and says why.
func TestARefusedWriteNeverRunsKubectl(t *testing.T) {
	var stderr bytes.Buffer
	code := run(strings.Fields("-n kube-system apply -f -"), strings.NewReader(`{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"aws-node","namespace":"kube-system"}}`), &bytes.Buffer{}, &stderr)
	if code != 3 || !strings.Contains(stderr.String(), "harness write guard") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	stderr.Reset()
	if code := run(strings.Fields("label node cp a="), strings.NewReader(""), &bytes.Buffer{}, &stderr); code != 3 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestStdinIsReadOnlyForManifests(t *testing.T) {
	for args, want := range map[string]bool{"apply -f -": true, "create --filename=-": true, "get pods": false, "apply -f manifest.yaml": false} {
		if got := readsStdin(strings.Fields(args)); got != want {
			t.Errorf("%s: %v", args, got)
		}
	}
}

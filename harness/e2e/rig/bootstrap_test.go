package rig

import (
	"context"
	"testing"
)

type legacyBootstrapNode struct {
	Node
	called bool
}

func (*legacyBootstrapNode) Name() string { return "worker" }
func (n *legacyBootstrapNode) Userdata(context.Context, []byte) error {
	n.called = true
	return nil
}

type nativeBootstrapNode struct {
	legacyBootstrapNode
	got BootstrapData
}

func (n *nativeBootstrapNode) Bootstrap(_ context.Context, data BootstrapData) error {
	n.got = data
	return nil
}

func TestUnsupportedBootstrapDoesNotMutateMachine(t *testing.T) {
	for _, format := range []string{"", "ignition", "unknown"} {
		n := &legacyBootstrapNode{}
		if err := Bootstrap(context.Background(), n, BootstrapData{Format: format, Value: []byte("payload")}); err == nil {
			t.Fatalf("accepted unsupported format %q", format)
		}
		if n.called {
			t.Fatal("unsupported data reached first boot")
		}
	}
}

func TestNativeBootstrapReceivesCAPIFormatUnchanged(t *testing.T) {
	n := &nativeBootstrapNode{}
	data := BootstrapData{Format: Ignition, Value: []byte(`{"ignition":{"version":"3.5.0"}}`)}
	if err := Bootstrap(context.Background(), n, data); err != nil {
		t.Fatal(err)
	}
	if n.called || n.got.Format != data.Format || string(n.got.Value) != string(data.Value) {
		t.Fatal("native bootstrap was reinterpreted as legacy cloud-config")
	}
}

type instanceBootstrapNode struct {
	nativeBootstrapNode
	uid string
}

func (n *instanceBootstrapNode) BootstrapInstance(_ context.Context, uid string, data BootstrapData) error {
	n.uid, n.got = uid, data
	return nil
}

func TestInstanceBootstrapDispatchPreservesIdentityAndFormat(t *testing.T) {
	n := &instanceBootstrapNode{}
	data := BootstrapData{Format: Ignition, Value: []byte(`{"ignition":{"version":"3.5.0"}}`)}
	if err := BootstrapInstance(context.Background(), n, "infra-uid", data); err != nil {
		t.Fatal(err)
	}
	if n.uid != "infra-uid" || n.got.Format != data.Format || string(n.got.Value) != string(data.Value) || n.called {
		t.Fatal("instance bootstrap lost identity or changed the native document")
	}
	n.uid = ""
	if err := BootstrapInstance(context.Background(), n, "", data); err == nil || n.uid != "" {
		t.Fatal("accepted missing instance identity")
	}
}

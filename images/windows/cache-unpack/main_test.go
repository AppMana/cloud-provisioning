package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"io"

	"strings"
	"testing"
)

func TestPinnedTarget(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for _, ref := range []string{"registry.example/image@" + digest, "registry.example:5000/image:tag@" + digest} {
		got, err := pinnedTarget(ref)
		if err != nil || got != digest {
			t.Fatalf("%q: %q %v", ref, got, err)
		}
	}
	for _, ref := range []string{"registry.example/image:latest", "registry.example/image@sha256:bad", "registry.example/image@" + digest + "@" + digest, "registry.example/a b@" + digest} {
		if _, err := pinnedTarget(ref); err == nil {
			t.Fatalf("accepted %q", ref)
		}
	}
}

// A nil embedded Image deliberately fails if production adds unexpected calls.
// These fakes model only the metadata/unpack/readiness contract observed on AWS.
type observedImage struct {
	containerd.Image
	name                    string
	target                  digest.Digest
	unpackError, checkError error
	ready                   bool
	unpacks                 int
}

func (i *observedImage) Name() string               { return i.name }
func (i *observedImage) Target() ocispec.Descriptor { return ocispec.Descriptor{Digest: i.target} }
func (i *observedImage) Unpack(_ context.Context, snapshotter string, _ ...containerd.UnpackOpt) error {
	if snapshotter != "windows" {
		panic("unexpected snapshotter")
	}
	i.unpacks++
	return i.unpackError
}
func (i *observedImage) IsUnpacked(_ context.Context, snapshotter string) (bool, error) {
	if snapshotter != "windows" {
		panic("unexpected snapshotter")
	}
	return i.ready, i.checkError
}
func TestObservedUnpackSuccessRequiresNativeReadiness(t *testing.T) {
	target := digest.Digest("sha256:" + strings.Repeat("a", 64))
	name := "quay.io/k0sproject/apiserver-network-proxy-agent@" + target.String()
	for _, ready := range []bool{false, true} {
		image := &observedImage{name: name, target: target, ready: ready}
		var out bytes.Buffer
		err := unpackExisting(context.Background(), []string{name}, func(context.Context, string) (containerd.Image, error) { return image, nil }, &out)
		if (err == nil) != ready || image.unpacks != 1 {
			t.Fatalf("ready=%v: calls=%d error=%v", ready, image.unpacks, err)
		}
		if !ready && out.Len() != 0 {
			t.Fatal("failed readiness emitted a success record")
		}
		if ready {
			var record map[string]any
			if json.Unmarshal(out.Bytes(), &record) != nil || record["unpacked"] != true || record["target"] != target.String() {
				t.Fatal("missing qualified receipt")
			}
		}
	}
}
func TestAllReferenceTargetsPreflightBeforeAnyUnpack(t *testing.T) {
	target := digest.Digest("sha256:" + strings.Repeat("a", 64))
	names := []string{"registry.example/first@" + target.String(), "registry.example/second@" + target.String()}
	first := &observedImage{name: names[0], target: target, ready: true}
	second := &observedImage{name: names[1], target: digest.Digest("sha256:" + strings.Repeat("b", 64)), ready: true}
	err := unpackExisting(context.Background(), names, func(_ context.Context, name string) (containerd.Image, error) {
		if name == names[0] {
			return first, nil
		}
		return second, nil
	}, io.Discard)
	if err == nil || first.unpacks != 0 || second.unpacks != 0 {
		t.Fatal("target drift was not rejected before mutation")
	}
}
func TestUnpackAndReadbackFailuresRemainFailures(t *testing.T) {
	target := digest.Digest("sha256:" + strings.Repeat("a", 64))
	name := "registry.example/image@" + target.String()
	failure := errors.New("native operation failed")
	for _, image := range []*observedImage{{name: name, target: target, unpackError: failure}, {name: name, target: target, checkError: failure}} {
		err := unpackExisting(context.Background(), []string{name}, func(context.Context, string) (containerd.Image, error) { return image, nil }, io.Discard)
		if !errors.Is(err, failure) {
			t.Fatalf("lost failure: %v", err)
		}
	}
}

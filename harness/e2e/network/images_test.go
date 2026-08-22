package network

import (
	"strings"
	"testing"
)

// A manifest with no images is a failure, not a run that carries
// nothing in. It would mean the fetch returned something other than
// the manifest, and every pod would then pull from a registry the
// site cannot reach.
func TestAManifestWithNoImagesIsCaught(t *testing.T) {
	if got := ImagesIn([]byte("apiVersion: v1\nkind: List\n")); len(got) != 0 {
		t.Errorf("found images in a manifest that has none: %v", got)
	}
}

// The line must be an image field, not any line mentioning one.
func TestOnlyImageFieldsAreRead(t *testing.T) {
	manifest := `
      # image: docker.io/calico/node:v3.29.0 was the old one
      annotations:
        note: "see image: something"
        image: docker.io/calico/node:v3.29.1
`
	got := ImagesIn([]byte(manifest))
	for _, g := range got {
		if strings.Contains(g, "v3.29.0") {
			t.Errorf("a commented-out image was read: %v", got)
		}
	}
}

// A digest and a tag naming the same image are one image. Carried
// twice, the second copy is not wrong so much as slow — and on the
// machine rig every image crosses an ssh channel.
func TestADigestAndItsTagAreOneImage(t *testing.T) {
	got := ImagesIn([]byte("      image: quay.io/cilium/cilium:v1.16.5@sha256:abc\n"))
	if len(got) != 1 || got[0] != "quay.io/cilium/cilium:v1.16.5" {
		t.Errorf("read %v, want the image without its digest", got)
	}
}

// Every image the manifest names has to be carried in, because the
// site has no route to a registry. Missing one leaves pods pulling
// forever on a node that looks healthy in every other respect.
func TestEveryImageInTheManifestIsFound(t *testing.T) {
	manifest := `
      containers:
        - name: upgrade-ipam
          image: docker.io/calico/cni:v3.29.1
        - name: install-cni
          image: docker.io/calico/cni:v3.29.1
        - name: calico-node
          image: "docker.io/calico/node:v3.29.1"
      initContainers:
        - image: docker.io/calico/kube-controllers:v3.29.1
`
	got := ImagesIn([]byte(manifest))
	if len(got) != 3 {
		t.Fatalf("found %d images, want 3 distinct ones: %v", len(got), got)
	}
	for _, want := range []string{
		"docker.io/calico/cni:v3.29.1",
		"docker.io/calico/node:v3.29.1",
		"docker.io/calico/kube-controllers:v3.29.1",
	} {
		var found bool
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was not found: %v", want, got)
		}
	}
	// Quoted values must not keep their quotes: an image name with a
	// quote in it is not an image name.
	for _, g := range got {
		if strings.ContainsAny(g, `"'`) {
			t.Errorf("image %q kept its quotes", g)
		}
	}
}

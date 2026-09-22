package k0s

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	native "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"sigs.k8s.io/yaml"
)

func pinnedTestImages() *native.ClusterImages {
	image := func(name string) *native.ImageSpec {
		return &native.ImageSpec{Image: "ghcr.io/appmana/" + name, Version: "aligned@sha256:" + strings.Repeat("a", 64)}
	}
	return &native.ClusterImages{DefaultPullPolicy: "Never", KubeProxy: image("kube-proxy"), Windows: &native.WindowsImageSpec{KubeProxy: image("kube-proxy-windows"), Pause: image("pause-windows")}, Calico: &native.CalicoImageSpec{
		CNI: image("cni"), Node: image("node"), KubeControllers: image("kube-controllers"),
		Windows: &native.CalicoWindowsImageSpec{CNI: image("cni-windows"), Node: image("node-windows")},
	}}
}

func TestCalicoImagePinsAreRequiredBeforeGuestMutation(t *testing.T) {
	if err := (Builder{}).Build(context.Background(), cluster.Deps{Network: "calico"}); err == nil || !strings.Contains(err.Error(), "explicit native image") {
		t.Fatalf("missing pins not rejected first: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*native.ClusterImages)
	}{
		{"missing node", func(i *native.ClusterImages) { i.Calico.Node = nil }},
		{"tag only", func(i *native.ClusterImages) { i.Calico.CNI.Version = "latest" }},
		{"pull enabled", func(i *native.ClusterImages) { i.DefaultPullPolicy = "IfNotPresent" }},
		{"partial Windows", func(i *native.ClusterImages) { i.Calico.Windows = &native.CalicoWindowsImageSpec{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			images := pinnedTestImages()
			test.change(images)
			if err := ValidateImages("calico", images); err == nil {
				t.Fatal("accepted incomplete fork settings")
			}
		})
	}
}

func TestNativeImageSettingsSurviveConfigWithoutMutation(t *testing.T) {
	images := pinnedTestImages()
	images.CoreDNS = &native.ImageSpec{Image: "example.invalid/dns", Version: "explicit"}
	before := images.DeepCopy()
	node := &configNode{}
	d := cluster.Deps{Topology: lab.Default(), Rig: configRig{node: node}, Network: "calico", K0sImages: images}
	if err := (Builder{}).config(context.Background(), d, d.Topology.MustNode("cp")); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(images, before) {
		t.Fatal("mutated native caller settings")
	}
	var decoded struct {
		Spec struct{ Images *native.ClusterImages }
	}
	if err := yaml.Unmarshal(node.config, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Spec.Images, images) {
		t.Fatalf("native images changed: %+v", decoded.Spec.Images)
	}
}

func TestReadImagesDoesNotInsertNativeDefaults(t *testing.T) {
	images := pinnedTestImages()
	images.DefaultPullPolicy = ""
	body, err := json.Marshal(images)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "images.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadImages(context.Background(), path, fmt.Sprintf("%x", sha256.Sum256(body)))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.DefaultPullPolicy != "" {
		t.Fatal("injected an image pull default")
	}
	if err := ValidateImages("calico", decoded); err == nil {
		t.Fatal("accepted missing explicit pull policy")
	}
}

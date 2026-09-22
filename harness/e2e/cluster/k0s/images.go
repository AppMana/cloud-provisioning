package k0s

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/appmana/labcontainers/pkg/artifact"
	native "github.com/k0sproject/k0s/pkg/apis/k0s/v1beta1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// ReadImages decodes the native type without applying k0s's JSON defaults or
// repository rewriting. Go callers can construct ClusterImages directly.
func ReadImages(ctx context.Context, path, digest string) (*native.ClusterImages, error) {
	body, err := artifact.ReadFile(ctx, path, digest)
	if err != nil {
		return nil, err
	}
	type rawImages native.ClusterImages
	var images *rawImages
	if err := json.Unmarshal(body, &images); err != nil {
		return nil, err
	}
	if images == nil {
		return nil, fmt.Errorf("native k0s image configuration is required")
	}
	return (*native.ClusterImages)(images), nil
}

var pinnedVersion = regexp.MustCompile(`@sha256:[0-9a-fA-F]{64}$`)

// ValidateImages prevents implicit bundled upstream Calico in test profiles.
// Content pins do not prove fork provenance or compatible component versions.
func ValidateImages(network string, images *native.ClusterImages) error {
	calico := network == "calico" || network == "calico-site-bgp"
	if images == nil {
		if calico {
			return fmt.Errorf("k0s Calico profiles require explicit native image configuration from the aligned forks")
		}
		return nil
	}
	if errs := images.Validate(field.NewPath("images")); len(errs) != 0 {
		return errs.ToAggregate()
	}
	if !calico {
		return nil
	}
	if images.DefaultPullPolicy != "Never" {
		return fmt.Errorf("k0s Calico profiles require images.default_pull_policy=Never and preloaded images")
	}
	if images.Calico == nil {
		return fmt.Errorf("explicit Calico images are required")
	}
	check := func(name string, image *native.ImageSpec) error {
		if image == nil || !pinnedVersion.MatchString(image.Version) {
			return fmt.Errorf("explicit SHA256-pinned k0s image %s is required", name)
		}
		return nil
	}
	for _, item := range []struct {
		name  string
		image *native.ImageSpec
	}{
		{"calico.cni", images.Calico.CNI}, {"calico.node", images.Calico.Node},
		{"calico.kubecontrollers", images.Calico.KubeControllers}, {"kubeproxy", images.KubeProxy},
	} {
		if err := check(item.name, item.image); err != nil {
			return err
		}
	}
	// k0s detects Windows nodes dynamically; omitted Windows overrides could
	// introduce bundled upstream builds when an ad hoc worker joins later.
	{
		if images.Calico.Windows == nil || images.Windows == nil {
			return fmt.Errorf("explicit Windows Calico and Windows Kubernetes image settings are required for dynamically joined Windows nodes")
		}
		for _, item := range []struct {
			name  string
			image *native.ImageSpec
		}{
			{"calico.windows.cni", images.Calico.Windows.CNI}, {"calico.windows.node", images.Calico.Windows.Node},
			{"windows.kubeproxy", images.Windows.KubeProxy}, {"windows.pause", images.Windows.Pause},
		} {
			if err := check(item.name, item.image); err != nil {
				return err
			}
		}
	}
	return nil
}

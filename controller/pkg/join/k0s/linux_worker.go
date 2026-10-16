package k0s

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var workerDigest = regexp.MustCompile(`^[a-fA-F0-9]{64}$`)

// linuxWorkerValues admits only a baked executable from the observed base
// release. Bootstrap must verify its bytes before running it; there is no
// download fallback for an explicitly selected worker build.
func (p *Provider) linuxWorkerValues(ctx context.Context, base string) (map[string]any, error) {
	values := map[string]any{"k0sWorkerVersion": "", "k0sWorkerSHA256Amd64": "", "k0sWorkerSHA256Arm64": ""}
	if p.ConfigName == "" {
		return values, nil
	}
	if p.Client == nil {
		return nil, fmt.Errorf("reading k0s worker configuration requires a Kubernetes client")
	}
	cfg, err := p.Client.CoreV1().Secrets(p.ConfigNamespace).Get(ctx, p.ConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return values, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading k0s worker configuration: %w", err)
	}
	version, hasVersion := cfg.Data["worker-version"]
	amd, hasAMD := cfg.Data["worker-sha256-amd64"]
	arm, hasARM := cfg.Data["worker-sha256-arm64"]
	if !hasVersion && !hasAMD && !hasARM {
		return values, nil
	}
	match := observedK0sRelease.FindSubmatch(version)
	if !hasVersion || len(match) != 2 || string(match[1]) != base || !upstreamK0sRelease.MatchString(base) {
		return nil, fmt.Errorf("Linux worker-version must retain the exact observed base k0s release")
	}
	if !hasAMD && !hasARM || hasAMD && !workerDigest.Match(amd) || hasARM && !workerDigest.Match(arm) {
		return nil, fmt.Errorf("Linux worker override requires a SHA256 for each admitted architecture")
	}
	values["k0sWorkerVersion"] = string(version)
	values["k0sWorkerSHA256Amd64"] = strings.ToLower(string(amd))
	values["k0sWorkerSHA256Arm64"] = strings.ToLower(string(arm))
	return values, nil
}

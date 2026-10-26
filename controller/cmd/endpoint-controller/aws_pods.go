package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/appmana/cloud-provisioning/controller/pkg/awscredentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
	"github.com/appmana/cloud-provisioning/controller/pkg/cni/awspod"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/aws/aws-sdk-go-v2/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newAWSPodObserver(path, credentialsPath string, reader client.Reader) (*awspod.Observer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var scope awspod.Config
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&scope); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing AWS pod configuration")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	options := []func(*config.LoadOptions) error{config.WithRegion(scope.Region), config.WithRetryMaxAttempts(3)}
	if credentialsPath != "" {
		provider := aws.NewCredentialsCache(awscredentials.FileProvider{Path: credentialsPath}, func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = time.Minute })
		options = append(options, config.WithCredentialsProvider(provider))
	}
	cfg, err := config.LoadDefaultConfig(context.Background(), options...)
	if err != nil {
		return nil, err
	}
	api, err := attachmentaws.NewEC2Transport(cfg)
	if err != nil {
		return nil, err
	}
	return &awspod.Observer{Config: scope, Reader: reader, API: api}, nil
}

// Rebuild from authoritative hosts and this observation, never from the previous
// AllowedIPs: secondary pod IPs are /32s too and must disappear when released.
func nativeAllowedIPs(machine *unstructured.Unstructured, secret *corev1.Secret, prefixes []netip.Prefix) (string, error) {
	var hosts []string
	for _, key := range []string{join.WireGuardAddrAnnotation, join.WireGuardAddr6Annotation} {
		if raw := machine.GetAnnotations()[key]; raw != "" {
			p, err := netip.ParsePrefix(raw)
			if err != nil {
				return "", err
			}
			hosts = appendMissing(hosts, tunnel.HostCIDR(p.Addr().String()))
		}
	}
	if len(hosts) == 0 {
		return "", fmt.Errorf("Machine has no allocated tunnel host")
	}
	addresses, _, err := unstructured.NestedSlice(machine.Object, "status", "addresses")
	if err != nil {
		return "", err
	}
	_, machineAddresses := machineHosts(addresses)
	for _, ip := range machineAddresses {
		hosts = appendMissing(hosts, tunnel.HostCIDR(ip))
	}
	for _, p := range prefixes {
		hosts = appendMissing(hosts, p.String())
	}
	own := tunnel.PeerAllowedIPsPrefix + machine.GetName()
	for key, value := range secret.Data {
		if key == own || !strings.HasPrefix(key, tunnel.PeerAllowedIPsPrefix) {
			continue
		}
		for _, raw := range tunnel.SplitList(string(value)) {
			existing, err := netip.ParsePrefix(raw)
			if err != nil {
				return "", fmt.Errorf("invalid existing peer allocation")
			}
			for _, raw := range hosts {
				next, _ := netip.ParsePrefix(raw)
				if next.Overlaps(existing) {
					return "", fmt.Errorf("native allocation overlaps another mesh peer")
				}
			}
		}
	}
	// Site pod space is also an ownership boundary; it must never migrate to
	// a native peer because a provider range or allocation was misconfigured.
	for key, value := range secret.Data {
		if !strings.HasPrefix(key, tunnel.SitePodCIDRsPrefix) {
			continue
		}
		for _, raw := range tunnel.SplitList(string(value)) {
			site, err := netip.ParsePrefix(raw)
			if err != nil {
				return "", fmt.Errorf("invalid site allocation")
			}
			for _, p := range prefixes {
				if p.Overlaps(site) {
					return "", fmt.Errorf("native allocation overlaps site pod space")
				}
			}
		}
	}
	return strings.Join(hosts, ","), nil
}

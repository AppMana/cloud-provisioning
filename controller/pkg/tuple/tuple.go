// Package tuple models, renders, and validates platform tuples (such as
// "k0s + calico,bgp + aws") for mixed-OS BGP clusters and cloud-provisioning.
package tuple

import (
	"fmt"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/cni"
	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
)

// Tuple identifies a user's chosen distribution, site CNI + encapsulation,
// cloud provider, and optional guest OS / cloud CNI specialization.
//
// Canonical string syntax:
//   - "k0s + calico,bgp + aws"
//   - "k0s + calico,bgp,windows + aws"
//   - "k0s + calico,bgp + aws,aws-vpc"
//   - "k0s + calico,bgp + aws,site"
type Tuple struct {
	Distribution  string `json:"distribution"`
	CNI           string `json:"cni"`
	Encapsulation string `json:"encapsulation,omitempty"`
	Provider      string `json:"provider"`
	GuestOS       string `json:"guestOS,omitempty"`
	CloudCNI      string `json:"cloudCNI,omitempty"`
}

// Parse parses a platform tuple expression such as "k0s + calico,bgp + aws".
func Parse(raw string) (Tuple, error) {
	parts := strings.Split(strings.TrimSpace(raw), "+")
	if len(parts) != 3 {
		return Tuple{}, fmt.Errorf("platform tuple %q must have 3 '+'-separated segments: <distribution> + <cni[,mode[,os]]> + <provider[,cloud-cni]> (e.g. \"k0s + calico,bgp + aws\")", raw)
	}
	dist := strings.ToLower(strings.TrimSpace(parts[0]))
	cniTokens := splitTokens(parts[1])
	providerTokens := splitTokens(parts[2])
	if dist == "" || len(cniTokens) == 0 || len(providerTokens) == 0 {
		return Tuple{}, fmt.Errorf("platform tuple %q contains an empty segment", raw)
	}

	t := Tuple{
		Distribution: dist,
		CNI:          cniTokens[0],
		Provider:     providerTokens[0],
		GuestOS:      "mixed",
	}
	for _, tok := range cniTokens[1:] {
		switch tok {
		case "bgp", "bird", "native":
			t.Encapsulation = "bgp"
		case "vxlan", "ipip", "wireguard":
			t.Encapsulation = tok
		case "linux", "windows", "mixed":
			t.GuestOS = tok
		default:
			return Tuple{}, fmt.Errorf("unknown CNI/OS qualifier %q in tuple %q", tok, raw)
		}
	}
	if t.Encapsulation == "" && t.CNI == "calico" {
		t.Encapsulation = "bgp"
	}

	for _, tok := range providerTokens[1:] {
		switch tok {
		case "aws-vpc", "awsvpc":
			t.CloudCNI = "aws-vpc"
		case "site":
			t.CloudCNI = "site"
		case "linux", "windows", "mixed":
			t.GuestOS = tok
		default:
			return Tuple{}, fmt.Errorf("unknown provider/cloud-CNI qualifier %q in tuple %q", tok, raw)
		}
	}
	if t.CloudCNI == "" {
		if t.Provider == "aws" {
			t.CloudCNI = "aws-vpc"
		} else {
			t.CloudCNI = "site"
		}
	}
	return t, t.Validate()
}

func splitTokens(segment string) []string {
	raw := strings.Split(segment, ",")
	var out []string
	for _, item := range raw {
		trimmed := strings.ToLower(strings.TrimSpace(item))
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// String returns the canonical tuple representation.
func (t Tuple) String() string {
	cniPart := t.CNI
	if t.Encapsulation != "" {
		cniPart += "," + t.Encapsulation
	}
	if t.GuestOS != "" && t.GuestOS != "mixed" {
		cniPart += "," + t.GuestOS
	}
	provPart := t.Provider
	if t.CloudCNI != "" && !(t.Provider == "aws" && t.CloudCNI == "aws-vpc") && !(t.Provider != "aws" && t.CloudCNI == "site") {
		provPart += "," + t.CloudCNI
	}
	return fmt.Sprintf("%s + %s + %s", t.Distribution, cniPart, provPart)
}

// GuestOSList returns the guest operating systems covered by this tuple.
func (t Tuple) GuestOSList() []string {
	switch t.GuestOS {
	case "linux":
		return []string{"linux"}
	case "windows":
		return []string{"windows"}
	default:
		return []string{"linux", "windows"}
	}
}

// IncludesWindows reports whether Windows nodes are part of the platform tuple.
func (t Tuple) IncludesWindows() bool {
	return t.GuestOS == "" || t.GuestOS == "mixed" || t.GuestOS == "windows"
}

// ExpectedEncapsulation maps the tuple's encapsulation token to cni.Encapsulation.
func (t Tuple) ExpectedEncapsulation() cni.Encapsulation {
	switch t.Encapsulation {
	case "", "bgp", "bird", "native":
		return cni.Native
	case "vxlan", "ipip", "wireguard":
		return cni.Encapsulated
	default:
		return cni.Unknown
	}
}

// Validate checks whether the platform tuple is supported by cloud-provisioning
// and k0s/Calico mixed-OS BGP.
func (t Tuple) Validate() error {
	switch t.Distribution {
	case "k0s", "kubeadm", "k3s", "rke2", "microk8s":
	default:
		return fmt.Errorf("unsupported Kubernetes distribution %q (supported: k0s, kubeadm, k3s, rke2, microk8s)", t.Distribution)
	}
	switch t.CNI {
	case "calico", "cilium", "flannel":
	default:
		return fmt.Errorf("unsupported site CNI %q (supported: calico, cilium, flannel)", t.CNI)
	}
	switch t.Provider {
	case "aws", "docker", "labcontainers":
	default:
		return fmt.Errorf("unsupported cloud provider %q (supported: aws, docker, labcontainers)", t.Provider)
	}
	switch t.GuestOS {
	case "", "linux", "windows", "mixed":
	default:
		return fmt.Errorf("unsupported guest OS %q (supported: linux, windows, mixed)", t.GuestOS)
	}
	cloudCNI := t.CloudCNI
	if cloudCNI == "" {
		if t.Provider == "aws" {
			cloudCNI = "aws-vpc"
		} else {
			cloudCNI = "site"
		}
	}
	if cloudCNI != "aws-vpc" && cloudCNI != "site" {
		return fmt.Errorf("unsupported cloud CNI %q (supported: aws-vpc, site)", cloudCNI)
	}
	if cloudCNI == "aws-vpc" && t.Provider != "aws" {
		return fmt.Errorf("cloud CNI aws-vpc requires provider aws (got %q)", t.Provider)
	}
	if t.IncludesWindows() {
		if t.Distribution != "k0s" {
			return fmt.Errorf("Windows cloud/site nodes require distribution k0s (got %q)", t.Distribution)
		}
		if t.CNI != "calico" {
			return fmt.Errorf("Windows nodes on %s require site CNI calico,bgp (got %s)", t.Distribution, t.CNI)
		}
		if t.Encapsulation != "" && t.Encapsulation != "bgp" && t.Encapsulation != "native" && t.Encapsulation != "bird" {
			return fmt.Errorf("Windows nodes with Calico on k0s require unencapsulated bgp mode (got %q)", t.Encapsulation)
		}
	}
	if t.CNI == "calico" && t.Encapsulation != "" && t.Encapsulation != "bgp" && t.Encapsulation != "native" && t.Encapsulation != "bird" {
		return fmt.Errorf("cloud-provisioning with Calico requires unencapsulated bgp mode (got %q)", t.Encapsulation)
	}
	for _, osName := range t.GuestOSList() {
		class := platform.Class{OS: osName, CNI: cloudCNI}
		if class.OS != "linux" && class.OS != "windows" {
			return fmt.Errorf("tuple %s has unsupported guest OS %q", t, osName)
		}
		if class.OS == "windows" && !class.Native() && t.CNI != cni.Calico {
			return fmt.Errorf("tuple %s rejected for guest OS %s: site CNI %s does not support Windows", t, osName, t.CNI)
		}
	}
	return nil
}

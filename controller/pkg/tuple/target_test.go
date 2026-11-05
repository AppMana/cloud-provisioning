package tuple

import (
	"reflect"
	"testing"
)

func TestParseClusterTargetConfig_MultiCAPI(t *testing.T) {
	cfg, err := ParseClusterTargetConfig("k0s + calico,bgp + aws,aws-vpc; gcp,site")
	if err != nil {
		t.Fatalf("ParseClusterTargetConfig: %v", err)
	}
	if cfg.Site.Distribution != "k0s" || cfg.Site.CNI != "calico" || cfg.Site.Encapsulation != "bgp" {
		t.Fatalf("unexpected site profile: %+v", cfg.Site)
	}
	if len(cfg.Targets) != 2 {
		t.Fatalf("len(Targets) = %d, want 2", len(cfg.Targets))
	}
	awsTarget, ok := cfg.TargetForProvider("aws")
	if !ok || awsTarget.CloudCNI != "aws-vpc" || awsTarget.ManagerNamespace != "capa-system" {
		t.Fatalf("unexpected aws target: %+v (ok=%v)", awsTarget, ok)
	}
	gcpTarget, ok := cfg.TargetForProvider("gcp")
	if !ok || gcpTarget.CloudCNI != "site" || gcpTarget.ManagerNamespace != "capg-system" {
		t.Fatalf("unexpected gcp target: %+v (ok=%v)", gcpTarget, ok)
	}
	wantNS := []string{"capa-system", "capg-system"}
	if got := cfg.ManagerNamespaces(); !reflect.DeepEqual(got, wantNS) {
		t.Fatalf("ManagerNamespaces() = %v, want %v", got, wantNS)
	}

	targetOnly, err := ParseClusterTargetConfig("aws,aws-vpc;gcp,site,linux")
	if err != nil {
		t.Fatalf("ParseClusterTargetConfig target-only: %v", err)
	}
	if len(targetOnly.Targets) != 2 || targetOnly.Targets[1].GuestOS != "linux" {
		t.Fatalf("unexpected targetOnly: %+v", targetOnly)
	}
}

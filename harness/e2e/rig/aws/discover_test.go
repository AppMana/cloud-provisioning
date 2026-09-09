package aws

import (
	"context"
	"encoding/json"
	"testing"
)

type discoveryAPI struct{ body json.RawMessage }

func (a discoveryAPI) Call(_ context.Context, _, _ string, _ map[string]any) (json.RawMessage, error) {
	return a.body, nil
}

func TestGuestDiscoveryUsesPlatformNotImageName(t *testing.T) {
	for _, tc := range []struct{ platform, details, document string }{
		{"windows", "Windows", "AWS-RunPowerShellScript"},
		{"", "Linux/UNIX", "AWS-RunShellScript"},
		{"", "Ubuntu Pro", "AWS-RunShellScript"},
		{"", "Mac OS", ""}, {"unknown", "Linux/UNIX", ""}, {"", "", ""},
	} {
		body, _ := json.Marshal(map[string]any{"Reservations": []any{map[string]any{"Instances": []any{map[string]any{
			"InstanceId": "i-test", "Platform": tc.platform, "PlatformDetails": tc.details, "ImageId": "ami-misleading-name",
		}}}}})
		commands, err := DiscoverGuestCommands(context.Background(), discoveryAPI{body}, "i-test")
		if tc.document == "" {
			if err == nil {
				t.Fatalf("accepted unknown platform %+v", tc)
			}
			continue
		}
		if err != nil || commands.Document() != tc.document {
			t.Fatalf("%+v: %v %v", tc, commands, err)
		}
	}
	for _, body := range []string{`{}`, `not json`, `{"Reservations":[{"Instances":[{"InstanceId":"i-other","Platform":"windows"}]}]}`} {
		if _, err := DiscoverGuestCommands(context.Background(), discoveryAPI{json.RawMessage(body)}, "i-test"); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

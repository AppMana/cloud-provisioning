package scope

import "testing"

func TestPowerShellUsesSecretsAndNeverCompressesEC2Envelope(t *testing.T) {
	m, err := setupMachineScope()
	if err != nil {
		t.Fatal(err)
	}
	if !m.UseSecretsManager("powershell") {
		t.Fatal("Windows payload bypassed encrypted secret storage")
	}
	for _, setting := range []*bool{nil, boolPointer(false), boolPointer(true)} {
		m.AWSMachine.Spec.UncompressedUserData = setting
		if m.CompressUserData("powershell") {
			t.Fatal("EC2Launch received a gzip stream instead of its native document")
		}
	}
}
func boolPointer(value bool) *bool { return &value }

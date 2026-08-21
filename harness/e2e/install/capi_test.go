package install

import (
	"strings"
	"testing"
)

// Cluster API's published components.yaml is a template, not a
// manifest. clusterctl substitutes its variables at install time; an
// operator following the README runs clusterctl, so a lab that
// applies the file raw is not installing what an operator installs.
//
// Applied raw, the controller receives the literal
// "${CAPI_INSECURE_DIAGNOSTICS:=false}" as a flag value and
// crash-loops on ParseBool — which reads as Cluster API being broken
// rather than as the manifest being a template.
func TestClusterctlVariablesAreResolved(t *testing.T) {
	manifest := []byte(`args:
  - "--insecure-diagnostics=${CAPI_INSECURE_DIAGNOSTICS:=false}"
  - "--diagnostics-address=${CAPI_DIAGNOSTICS_ADDRESS:=:8443}"
  - "--feature-gates=MachinePool=${EXP_MACHINE_POOL:=true}"
`)
	got := string(SubstituteDefaults(manifest))

	if strings.Contains(got, "${") {
		t.Errorf("a variable survived substitution:\n%s", got)
	}
	for _, want := range []string{
		"--insecure-diagnostics=false",
		"--diagnostics-address=:8443",
		"MachinePool=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%q is missing from:\n%s", want, got)
		}
	}
}

// A variable with no default is left alone, so something genuinely
// unset fails loudly rather than silently becoming an empty string
// that means something else.
func TestAVariableWithNoDefaultIsLeftAlone(t *testing.T) {
	got := string(SubstituteDefaults([]byte("value: ${SOMETHING_REQUIRED}")))
	if !strings.Contains(got, "${SOMETHING_REQUIRED}") {
		t.Errorf("a variable with no default was substituted away: %s", got)
	}
}

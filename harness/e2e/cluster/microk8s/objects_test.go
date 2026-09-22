package microk8s

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestProviderSecretPreservesNativeCredentialData(t *testing.T) {
	config := []byte("opaque: data\nwith \"quotes\" and unicode λ")
	secret := providerSecret(config)
	if secret.Kind != "Secret" || secret.APIVersion != "v1" || secret.Name != "microk8s-provider-config" || secret.Namespace != "cloud-provisioning" || secret.Type != corev1.SecretTypeOpaque {
		t.Fatal("native secret metadata changed")
	}
	data, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	var got corev1.Secret
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.StringData["config.json"] != string(config) || len(got.Data) != 0 {
		t.Fatal("credential payload changed or double-encoded")
	}
}

package aws

import "testing"

// A failed call says which AWS error it was, without the rest of the
// message, which can echo the command's parameters.
func TestAFailedCallNamesOnlyItsErrorCode(t *testing.T) {
	for stderr, want := range map[string]string{
		"\nAn error occurred (ThrottlingException) when calling the SendCommand operation (reached max retries: 2): Rate exceeded\n": "ThrottlingException",
		"An error occurred (InvalidInstanceId) when calling the SendCommand operation: Instances [[i-0]] not in a valid state\n":     "InvalidInstanceId",
		"Parameter validation failed: commands=[secret]": "",
	} {
		if got := errorCode(stderr); got != want {
			t.Errorf("errorCode(%q) = %q, want %q", stderr, got, want)
		}
	}
}

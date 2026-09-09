package main

import (
	"crypto/sha256"
	"fmt"
)

// The lock follows the mesh Secret, rather than a Deployment name, so two
// releases configured to manage the same mesh cannot become separate writers.
func meshLeaderElectionID(secretName string) string {
	hash := sha256.Sum256([]byte(secretName))
	return fmt.Sprintf("cloud-provisioning-mesh-%x", hash[:16])
}

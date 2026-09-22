// Package containernet preserves the old import path during migration.
package containernet

import joinlabcontainers "github.com/appmana/cloud-provisioning/controller/pkg/join/labcontainers"

// Provider is retained for source compatibility. New code imports
// pkg/join/labcontainers.
type Provider = joinlabcontainers.Provider

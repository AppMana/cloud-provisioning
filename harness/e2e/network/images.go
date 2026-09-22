package network

import (
	"regexp"
	"sort"
	"strings"
)

// Both forms occur in a real manifest: image as a later key of a list
// item, and image as its first, where YAML puts the dash on the same
// line.
var imageLine = regexp.MustCompile(`(?m)^\s*-?\s*image:\s*(\S+)\s*$`)

// ImagesIn lists every image a manifest names, deduplicated.
//
// Shared, because every network is carried in the same way and a
// second copy of this regex would be a second place for a manifest's
// spelling to be missed — and the failure is a pod that never pulls,
// on a node with no route to a registry, which reads as the network
// being broken.
func ImagesIn(manifest []byte) []string {
	seen := map[string]bool{}
	for _, m := range imageLine.FindAllStringSubmatch(string(manifest), -1) {
		image := strings.Trim(m[1], `"'`)
		// Preserve digest identity: tags can move independently of the pin.
		seen[image] = true
	}
	out := make([]string, 0, len(seen))
	for image := range seen {
		out = append(out, image)
	}
	sort.Strings(out)
	return out
}

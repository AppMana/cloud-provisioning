package rig

import (
	"runtime/debug"
	"testing"
)

func TestDaemonProvenanceRejectsStaleAndReplacedSDK(t *testing.T) {
	module := "github.com/appmana/labcontainers"
	for _, tc := range []struct {
		name string
		info debug.BuildInfo
		want bool
	}{
		{"pinned module", debug.BuildInfo{Main: debug.Module{Path: module, Version: "v0.2.0-alpha.2.0.20261005235206-" + LabcontainersRevision[:12]}}, true},
		{"stale", debug.BuildInfo{Main: debug.Module{Path: module, Version: "v0.2.0-alpha.2"}}, false},
		{"local replacement", debug.BuildInfo{Deps: []*debug.Module{{Path: module, Version: "v0.0.0-" + LabcontainersRevision[:12], Replace: &debug.Module{Path: "/tmp/source"}}}}, false},
		{"clean checkout", debug.BuildInfo{Main: debug.Module{Path: module}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: LabcontainersRevision}, {Key: "vcs.modified", Value: "false"}}}, true},
		{"dirty checkout", debug.BuildInfo{Main: debug.Module{Path: module}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: LabcontainersRevision}, {Key: "vcs.modified", Value: "true"}}}, false},
		{"unknown", debug.BuildInfo{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesLabcontainers(&tc.info); got != tc.want {
				t.Fatalf("accepted=%v want=%v", got, tc.want)
			}
		})
	}
}

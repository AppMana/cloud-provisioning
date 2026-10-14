package rig

import (
	"debug/buildinfo"
	"fmt"
	"runtime/debug"
	"strings"
)

const LabcontainersRevision = "208b63a3311d988688cd73b3a010d0b8708da4ef"

func matchesLabcontainers(info *debug.BuildInfo) bool {
	const module = "github.com/appmana/labcontainers"
	for _, dep := range append([]*debug.Module{&info.Main}, info.Deps...) {
		if dep.Path == module && dep.Replace == nil && strings.HasSuffix(dep.Version, "-"+LabcontainersRevision[:12]) {
			return true
		}
	}
	if info.Main.Path != module {
		return false
	}
	revision, dirty := "", false
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			revision = setting.Value
		}
		if setting.Key == "vcs.modified" {
			dirty = setting.Value != "false"
		}
	}
	return revision == LabcontainersRevision && !dirty
}

func verifiedLabd(path string) (string, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read daemon provenance %s: %w", path, err)
	}
	if !matchesLabcontainers(info) {
		return "", fmt.Errorf("daemon %s does not match labcontainers SDK %s; rebuild it from the pinned source", path, LabcontainersRevision)
	}
	return path, nil
}

package main

import (
	"fmt"
	"runtime/debug"
	"strings"
	"time"
)

// These values are intentionally package variables so release builds can set
// them with -ldflags "-X main.version=... -X main.revision=... -X main.buildTime=...".
var (
	version   string
	revision  string
	buildTime string
)

var readBuildInfo = debug.ReadBuildInfo

func versionOutput() string {
	info, available := readBuildInfo()
	settings := buildSettings(info, available)

	resolvedVersion := safeBuildIdentifier(version)
	if resolvedVersion == "" && available && info != nil {
		resolvedVersion = safeBuildIdentifier(info.Main.Version)
	}
	if resolvedVersion == "" {
		resolvedVersion = "dev"
	}

	vcsRevision := safeBuildRevision(settings["vcs.revision"])
	resolvedRevision := safeBuildRevision(revision)
	injectedRevision := resolvedRevision != ""
	if resolvedRevision == "" {
		resolvedRevision = vcsRevision
	}
	if resolvedRevision == "" {
		resolvedRevision = "unknown"
	}

	resolvedBuildTime := normalizedBuildTime(buildTime)
	if resolvedBuildTime == "" && vcsRevision != "" && (!injectedRevision || resolvedRevision == vcsRevision) {
		resolvedBuildTime = normalizedBuildTime(settings["vcs.time"])
	}
	if resolvedBuildTime == "" {
		resolvedBuildTime = "unknown"
	}

	modified := "unknown"
	if vcsRevision != "" && resolvedRevision == vcsRevision {
		if value := settings["vcs.modified"]; value == "true" || value == "false" {
			modified = value
		}
	}

	return fmt.Sprintf(
		"xagent version=%s revision=%s build_time=%s modified=%s\n",
		resolvedVersion, resolvedRevision, resolvedBuildTime, modified,
	)
}

func buildSettings(info *debug.BuildInfo, available bool) map[string]string {
	settings := make(map[string]string, 3)
	if !available || info == nil {
		return settings
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision", "vcs.time", "vcs.modified":
			if _, exists := settings[setting.Key]; !exists {
				settings[setting.Key] = strings.TrimSpace(setting.Value)
			}
		}
	}
	return settings
}

func safeBuildIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, current := range value {
		if (current >= 'a' && current <= 'z') || (current >= 'A' && current <= 'Z') ||
			(current >= '0' && current <= '9') || current == '.' || current == '_' || current == '+' || current == '-' {
			continue
		}
		return ""
	}
	return value
}

func safeBuildRevision(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 40 && len(value) != 64 {
		return ""
	}
	for _, current := range value {
		if (current < '0' || current > '9') && (current < 'a' || current > 'f') {
			return ""
		}
	}
	return value
}

func normalizedBuildTime(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339)
}

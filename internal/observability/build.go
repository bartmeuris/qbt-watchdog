package observability

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Build describes the running binary. It is derived from Go's native build
// information (runtime/debug.ReadBuildInfo) rather than ldflags injection, so a
// binary built from a VCS checkout self-describes its source revision, commit
// time, dirty state, Go version and target platform.
type Build struct {
	Version    string `json:"version"`
	Revision   string `json:"revision"`
	CommitTime string `json:"commit_time"`
	Modified   *bool  `json:"modified,omitempty"` // nil = unknown, must NOT imply clean
	VCS        string `json:"vcs,omitempty"`
	GoVersion  string `json:"go_version"`
	Platform   string `json:"platform"`
}

// NewBuild reads the running binary's build information and returns its Build.
func NewBuild() Build {
	info, _ := debug.ReadBuildInfo()
	return buildFrom(info)
}

// Summary renders a one-line, human-readable build description for CLI help
// and diagnostics. Revision is shortened to 12 characters; unknown values are
// rendered as "-".
func (b Build) Summary() string {
	return fmt.Sprintf("%s revision %s %s %s", b.Version, revisionOrDash(b.Revision), b.GoVersion, b.Platform)
}

// revisionOrDash shortens a known revision and renders an unknown one as "-".
// shortRevision is reused rather than duplicating the 12-character rule.
func revisionOrDash(revision string) string {
	if !knownRevision(revision) {
		return "-"
	}
	return shortRevision(revision)
}

// knownRevision reports whether a revision carries real information. Go stamps
// the literal "unknown" when VCS metadata is absent, which must be treated the
// same as an empty value.
func knownRevision(revision string) bool { return revision != "" && revision != "unknown" }

// buildFrom parses a debug.BuildInfo into a Build. It is side-effect free so
// tests can pass synthetic fixtures; GoVersion/Platform come from the runtime.
// A nil info yields the same shape as a binary built without VCS stamping.
func buildFrom(info *debug.BuildInfo) Build {
	build := Build{
		Version:    "dev",
		Revision:   "unknown",
		CommitTime: "unknown",
		GoVersion:  runtime.Version(),
		Platform:   runtime.GOOS + "/" + runtime.GOARCH,
	}
	if info == nil {
		return build
	}
	settings := settingsMap(info.Settings)
	build.Revision = valueOr(settings["vcs.revision"], "unknown")
	build.CommitTime = valueOr(settings["vcs.time"], "unknown")
	build.VCS = settings["vcs"]
	switch settings["vcs.modified"] {
	case "true":
		modified := true
		build.Modified = &modified
	case "false":
		modified := false
		build.Modified = &modified
	}
	build.Version = resolveVersion(info.Main.Version, build.Revision)
	return build
}

// resolveVersion prefers the module version Go stamped into the binary. For a
// main module built from a VCS checkout that is a real tag or pseudo-version
// (e.g. v1.2.3 or v0.0.0-20261004...-1df74e5). Go reports "(devel)" when no
// module version is available: with -buildvcs=false, when building outside a
// VCS checkout, on pre-1.24 toolchains, and in edge cases such as a nested
// module without a root go.mod. In those cases fall back to the short revision,
// or "dev" when even that is unknown.
func resolveVersion(moduleVersion, revision string) string {
	if moduleVersion != "" && moduleVersion != "(devel)" {
		return moduleVersion
	}
	if knownRevision(revision) {
		return shortRevision(revision)
	}
	return "dev"
}

func shortRevision(revision string) string {
	const short = 12
	if len(revision) > short {
		return revision[:short]
	}
	return revision
}

func settingsMap(settings []debug.BuildSetting) map[string]string {
	m := make(map[string]string, len(settings))
	for _, s := range settings {
		m[s.Key] = s.Value
	}
	return m
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

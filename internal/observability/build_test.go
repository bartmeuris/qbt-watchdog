package observability

import (
	"encoding/json"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestBuildFromFullMetadata(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
		Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: "1df74e57cab4e99ab440d987ecece0dee103c0b4"},
			{Key: "vcs.time", Value: "2026-10-04T20:21:34Z"},
			{Key: "vcs.modified", Value: "false"},
		},
	}
	build := buildFrom(info)
	if build.Version != "v1.2.3" {
		t.Fatalf("Version = %q, want v1.2.3", build.Version)
	}
	if build.Revision != "1df74e57cab4e99ab440d987ecece0dee103c0b4" {
		t.Fatalf("Revision = %q", build.Revision)
	}
	if build.CommitTime != "2026-10-04T20:21:34Z" {
		t.Fatalf("CommitTime = %q", build.CommitTime)
	}
	if build.VCS != "git" {
		t.Fatalf("VCS = %q, want git", build.VCS)
	}
	if build.Modified == nil || *build.Modified {
		t.Fatalf("Modified = %v, want false", build.Modified)
	}
	if build.GoVersion != runtime.Version() {
		t.Fatalf("GoVersion = %q, want %q", build.GoVersion, runtime.Version())
	}
	if want := runtime.GOOS + "/" + runtime.GOARCH; build.Platform != want {
		t.Fatalf("Platform = %q, want %q", build.Platform, want)
	}
}

func TestBuildFromDevelVersionFallsBackToShortRevision(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "1df74e57cab4e99ab440d987ecece0dee103c0b4"},
		},
	}
	build := buildFrom(info)
	if build.Version != "1df74e57cab4" {
		t.Fatalf("Version = %q, want 1df74e57cab4", build.Version)
	}
}

func TestBuildFromMissingVCSLeavesModifiedUnknown(t *testing.T) {
	info := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}
	build := buildFrom(info)
	if build.Modified != nil {
		t.Fatalf("Modified = %v, want nil when vcs.modified is absent", *build.Modified)
	}
	if build.Revision != "unknown" {
		t.Fatalf("Revision = %q, want unknown", build.Revision)
	}
	if build.CommitTime != "unknown" {
		t.Fatalf("CommitTime = %q, want unknown", build.CommitTime)
	}
	if build.VCS != "" {
		t.Fatalf("VCS = %q, want empty", build.VCS)
	}
	if build.Version != "dev" {
		t.Fatalf("Version = %q, want dev fallback", build.Version)
	}
}

func TestBuildFromModifiedTrue(t *testing.T) {
	info := &debug.BuildInfo{
		Main:     debug.Module{Version: "v1.2.3+dirty"},
		Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}},
	}
	build := buildFrom(info)
	if build.Modified == nil || !*build.Modified {
		t.Fatalf("Modified = %v, want true", build.Modified)
	}
}

func TestBuildFromMalformedModifiedLeavesUnknown(t *testing.T) {
	for _, value := range []string{"", "yes"} {
		t.Run("value="+value, func(t *testing.T) {
			info := &debug.BuildInfo{
				Main:     debug.Module{Version: "v1.2.3"},
				Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: value}},
			}
			build := buildFrom(info)
			if build.Modified != nil {
				t.Fatalf("Modified = %v, want nil for malformed %q", *build.Modified, value)
			}
		})
	}
}

func TestBuildJSONModifiedContract(t *testing.T) {
	unknown, err := json.Marshal(Build{Modified: nil})
	if err != nil {
		t.Fatalf("marshal unknown: %v", err)
	}
	if strings.Contains(string(unknown), `"modified"`) {
		t.Fatalf("nil Modified must omit key, got %s", unknown)
	}

	clean := false
	known, err := json.Marshal(Build{Modified: &clean})
	if err != nil {
		t.Fatalf("marshal known: %v", err)
	}
	if !strings.Contains(string(known), `"modified":false`) {
		t.Fatalf("false Modified must emit key, got %s", known)
	}
}

func TestBuildFromEmptySettingValuesFallBack(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{Version: "v1.2.3"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: ""},
			{Key: "vcs.time", Value: ""},
		},
	}
	build := buildFrom(info)
	if build.Revision != "unknown" {
		t.Fatalf("Revision = %q, want unknown", build.Revision)
	}
	if build.CommitTime != "unknown" {
		t.Fatalf("CommitTime = %q, want unknown", build.CommitTime)
	}
}

func TestBuildFromNilInfo(t *testing.T) {
	build := buildFrom(nil)
	if build.Version != "dev" || build.Revision != "unknown" || build.CommitTime != "unknown" {
		t.Fatalf("unexpected defaults: %+v", build)
	}
	if build.Modified != nil || build.VCS != "" {
		t.Fatalf("unexpected VCS state: %+v", build)
	}
	if build.GoVersion != runtime.Version() || build.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("unexpected runtime fields: %+v", build)
	}
}

func TestResolveVersionFallback(t *testing.T) {
	cases := []struct {
		name          string
		moduleVersion string
		revision      string
		want          string
	}{
		{"tag", "v1.2.3", "1df74e57cab4e99ab440d987ecece0dee103c0b4", "v1.2.3"},
		{"pseudo-version", "v0.0.0-20261004202134-1df74e57cab4", "1df74e57cab4e99ab440d987ecece0dee103c0b4", "v0.0.0-20261004202134-1df74e57cab4"},
		{"devel with revision", "(devel)", "1df74e57cab4e99ab440d987ecece0dee103c0b4", "1df74e57cab4"},
		{"empty module version with revision", "", "1df74e57cab4e99ab440d987ecece0dee103c0b4", "1df74e57cab4"},
		{"tag with empty revision", "v1.2.3", "", "v1.2.3"},
		{"devel with empty revision", "(devel)", "", "dev"},
		{"devel with short revision", "(devel)", "abc123", "abc123"},
		{"devel without revision", "(devel)", "unknown", "dev"},
		{"empty without revision", "", "unknown", "dev"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveVersion(tc.moduleVersion, tc.revision); got != tc.want {
				t.Fatalf("resolveVersion(%q, %q) = %q, want %q", tc.moduleVersion, tc.revision, got, tc.want)
			}
		})
	}
}

func TestNewBuildSelfDescribes(t *testing.T) {
	build := NewBuild()
	if build.Version == "" || build.GoVersion == "" || build.Platform == "" {
		t.Fatalf("NewBuild left fields empty: %+v", build)
	}
}

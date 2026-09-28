package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func watchFixture(t *testing.T, path string, process map[string]string) (*Manager, <-chan error) {
	t.Helper()
	load := func(path string) (Config, error) { return LoadWithEnvironment(path, process) }
	initial, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(path, initial)
	ctx, cancel := context.WithCancel(context.Background())
	outcomes := make(chan error, 64)
	done := make(chan error, 1)
	go func() {
		done <- watch(ctx, path, load, func(c Config) error { return m.Reload(c, nil) }, func(err error) {
			m.Record(err)
			outcomes <- err
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("dotenv watcher did not stop")
		}
	})
	awaitDotEnvOutcome(t, outcomes, false, 3*time.Second)
	return m, outcomes
}

func awaitDotEnvOutcome(t *testing.T, outcomes <-chan error, wantError bool, timeout time.Duration) {
	t.Helper()
	select {
	case err := <-outcomes:
		if (err != nil) != wantError {
			t.Fatal("unexpected dotenv reload outcome", err)
		}
		if err != nil && (strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), string(os.PathSeparator))) {
			t.Fatal("dotenv reload error leaked contents or path")
		}
	case <-time.After(timeout):
		t.Fatal("dotenv change was not reconciled")
	}
}

func TestDotEnvWatchRotationLastGoodAndDeletion(t *testing.T) {
	t.Parallel()
	path := writeFile(t, "config.yaml", minimal+"qbt_api_key: '${KEY}'\n")
	dotenv := filepath.Join(filepath.Dir(path), ".env")
	putFixture(t, dotenv, "KEY=SECRET_ORIGINAL\nSECRET_UNUSED_METADATA=SECRET_UNUSED_VALUE\n")
	m, outcomes := watchFixture(t, path, nil)
	original := m.Current()
	putFixture(t, dotenv, "KEY='SECRET_ROTATED'\n")
	awaitDotEnvOutcome(t, outcomes, false, 3*time.Second)
	if m.Current().APIKey != "SECRET_ROTATED" || m.Status().Generation != 2 || m.Current().SafetyKey() == original.SafetyKey() || m.Current().EndpointKey() != original.EndpointKey() {
		t.Fatal("dotenv rotation did not apply existing credential safety rules")
	}
	for _, invalid := range []string{"KEY='SECRET_UNCLOSED", "KEY=", "", "OTHER=SECRET_MISSING_KEY"} {
		putFixture(t, dotenv, invalid)
		awaitDotEnvOutcome(t, outcomes, true, 3*time.Second)
		if m.Current().APIKey != "SECRET_ROTATED" || m.Status().Generation != 2 || m.Status().Healthy() {
			t.Fatal("invalid dotenv did not retain last known good snapshot")
		}
	}
	replacement := filepath.Join(filepath.Dir(path), "replacement")
	putFixture(t, replacement, "KEY=SECRET_ATOMIC\n")
	if err := os.Rename(replacement, dotenv); err != nil {
		t.Fatal(err)
	}
	awaitDotEnvOutcome(t, outcomes, false, 3*time.Second)
	if m.Current().APIKey != "SECRET_ATOMIC" || m.Status().Generation != 3 || !m.Status().Healthy() {
		t.Fatal("atomic dotenv replacement did not recover")
	}
	if err := os.Remove(dotenv); err != nil {
		t.Fatal(err)
	}
	awaitDotEnvOutcome(t, outcomes, true, 3*time.Second)
	if m.Current().APIKey != "SECRET_ATOMIC" || m.Status().Generation != 3 {
		t.Fatal("dotenv deletion replaced referenced credentials")
	}
	for _, value := range []any{m.Current(), m.Status()} {
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), "SECRET") || strings.Contains(string(encoded), ".env") {
			t.Fatal("snapshot retained dotenv metadata or exposed credentials", err)
		}
	}
	putFixture(t, dotenv, "KEY=SECRET_RECOVERED\n")
	awaitDotEnvOutcome(t, outcomes, false, 3*time.Second)
	if m.Current().APIKey != "SECRET_RECOVERED" || m.Status().Generation != 4 || !m.Status().Healthy() {
		t.Fatal("dotenv recreation did not recover")
	}
}

func TestDotEnvWatchProcessSnapshotShadowsRotationAndDeletion(t *testing.T) {
	t.Parallel()
	path := writeFile(t, "config.yaml", minimal+"qbt_api_key: '${KEY}'\n")
	dotenv := filepath.Join(filepath.Dir(path), ".env")
	putFixture(t, dotenv, "KEY=SECRET_FILE\n")
	m, outcomes := watchFixture(t, path, map[string]string{"KEY": "SECRET_PROCESS"})
	putFixture(t, dotenv, "KEY=SECRET_ROTATED\n")
	awaitDotEnvOutcome(t, outcomes, false, 3*time.Second)
	if m.Current().APIKey != "SECRET_PROCESS" || m.Status().Generation != 1 {
		t.Fatal("dotenv rotation overrode process snapshot")
	}
	if err := os.Remove(dotenv); err != nil {
		t.Fatal(err)
	}
	awaitDotEnvOutcome(t, outcomes, false, 3*time.Second)
	if m.Current().APIKey != "SECRET_PROCESS" || m.Status().Generation != 1 || !m.Status().Healthy() {
		t.Fatal("optional dotenv deletion invalidated process-backed credentials")
	}
}

func TestDotEnvWatchPeriodicReconciliationForExternalSymlinkTarget(t *testing.T) {
	t.Parallel()
	path := writeFile(t, "config.yaml", minimal+"qbt_api_key: '${KEY}'\n")
	target := writeFile(t, "external.env", "KEY=SECRET_ORIGINAL\n")
	if err := os.Symlink(target, filepath.Join(filepath.Dir(path), ".env")); err != nil {
		t.Fatal(err)
	}
	m, outcomes := watchFixture(t, path, nil)
	// Neither the target nor its directory is watched: only the 5s fallback
	// can discover this replacement of the external mounted secret.
	putFixture(t, target+".new", "KEY=SECRET_RECONCILED\n")
	if err := os.Rename(target+".new", target); err != nil {
		t.Fatal(err)
	}
	awaitDotEnvOutcome(t, outcomes, false, 8*time.Second)
	if m.Current().APIKey != "SECRET_RECONCILED" || m.Status().Generation != 2 {
		t.Fatal("periodic reconciliation did not re-read dotenv")
	}
}

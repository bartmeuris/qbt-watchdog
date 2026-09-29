package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func manager(t *testing.T, body string) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return NewManager(path, c), path
}

func TestManagerPublishesGenerationsAndKeepsLastKnownGood(t *testing.T) {
	m, _ := manager(t, minimal)
	if status := m.Status(); status.Generation != 1 || !status.Healthy() {
		t.Fatal("startup is not generation 1", status)
	}
	next := m.Current()
	next.PollInterval = 90 * time.Second
	committed := Config{}
	if err := m.Reload(next, func(c Config) error { committed = c; return nil }); err != nil {
		t.Fatal(err)
	}
	if committed.PollInterval != 90*time.Second || m.Current().PollInterval != 90*time.Second {
		t.Fatal("accepted reload not published")
	}
	if status := m.Status(); status.Generation != 2 || !status.Healthy() || status.LastReloadAt.IsZero() {
		t.Fatal("generation did not advance", status)
	}

	good := m.Current()
	broken := good.Clone()
	broken.PollInterval = time.Second
	failure := errors.New("client rebuild failed")
	if err := m.Reload(broken, func(Config) error { return failure }); !errors.Is(err, failure) {
		t.Fatal("commit failure not surfaced", err)
	}
	if m.Current().PollInterval != good.PollInterval {
		t.Fatal("failed reload replaced the running configuration")
	}
	status := m.Status()
	if status.Generation != 2 || status.Healthy() || status.LastReloadError != failure.Error() {
		t.Fatal("failed reload not reported", status)
	}

	restart := good.Clone()
	restart.Listen = ":9999"
	if err := m.Reload(restart, func(Config) error { t.Fatal("restart-only candidate reached commit"); return nil }); err == nil {
		t.Fatal("restart-only change accepted")
	}
	if m.Current().Listen != good.Listen || m.Status().Generation != 2 {
		t.Fatal("restart-only change partially applied")
	}

	m.Record(nil)
	if !m.Status().Healthy() {
		t.Fatal("recovery not visible")
	}
}

func TestManagerSnapshotsAreIsolatedAndConcurrent(t *testing.T) {
	m, _ := manager(t, minimal)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				c := m.Current()
				c.URL.Host = "MUTATED"
				c.Policies[Metadata] = Policy{Action: DeleteFile, Threshold: time.Second, ArrMode: InheritArrMode}
				c.ExcludeTags = append(c.ExcludeTags, "mutated")
				_ = m.Status()
			}
		}()
	}
	for i := range 50 {
		next := m.Current()
		next.UIRefreshInterval = time.Duration(i+1) * time.Second
		if err := m.Reload(next, nil); err != nil {
			t.Error(err)
		}
	}
	wg.Wait()
	if c := m.Current(); c.URL.Host != "host" || c.Policies[Metadata].Action != Warn {
		t.Fatal("a reader mutated the published configuration", c.URL.Host)
	}
	if m.Status().Generation != 51 {
		t.Fatal("lost a generation", m.Status())
	}
}

func TestManagerRunAppliesLiveChangesAndSurvivesBadFiles(t *testing.T) {
	m, path := manager(t, minimal)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcomes := make(chan error, 32)
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx, nil, func(err error) { outcomes <- err }) }()
	await := func(wantFailure bool) error {
		t.Helper()
		select {
		case err := <-outcomes:
			if (err != nil) != wantFailure {
				t.Fatal("unexpected reload outcome", err)
			}
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("reload timed out")
			return nil
		}
	}
	replace := func(body string) {
		t.Helper()
		tmp := path + ".new"
		if err := os.WriteFile(tmp, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	await(false)

	// An atomic replacement must be noticed even though the inode changed.
	replace(minimal + "poll_interval: '13s'\nlog_level: 'debug'\n")
	await(false)
	if c := m.Current(); c.PollInterval != 13*time.Second || c.LogLevel != "debug" {
		t.Fatal("live change not applied", c.PollInterval, c.LogLevel)
	}
	generation := m.Status().Generation

	// An invalid file keeps the last known good configuration and reports.
	replace(minimal + "poll_interval: 'SECRET'\n")
	err := await(true)
	if strings.Contains(err.Error(), "SECRET") {
		t.Fatal("reload error echoed file content", err)
	}
	if c := m.Current(); c.PollInterval != 13*time.Second {
		t.Fatal("invalid reload replaced the running configuration")
	}
	status := m.Status()
	if status.Generation != generation || status.Healthy() || status.LastReloadError != err.Error() {
		t.Fatal("invalid reload not reported", status)
	}

	// A restart-only change is rejected while everything else is retained.
	replace(minimal + "poll_interval: '13s'\nlisten: ':9191'\n")
	if err := await(true); !strings.Contains(err.Error(), "listen") {
		t.Fatal("restart-only change not rejected by the watcher", err)
	}
	if m.Current().Listen != ":8080" {
		t.Fatal("restart-only change applied")
	}

	// Recovering clears the reported error and advances the generation.
	replace(minimal + "poll_interval: '17s'\n")
	await(false)
	if c, status := m.Current(), m.Status(); c.PollInterval != 17*time.Second || !status.Healthy() || status.Generation != generation+1 {
		t.Fatal("recovery not applied", c.PollInterval, status)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher leaked")
	}
}

func TestManagerRunIdenticalRecoveryClearsErrorWithoutCommit(t *testing.T) {
	m, path := manager(t, minimal)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outcomes := make(chan error, 16)
	commits := 0
	done := make(chan error, 1)
	go func() {
		done <- m.Run(ctx, func(Config) error { commits++; return nil }, func(err error) { outcomes <- err })
	}()
	await := func(wantFailure bool) error {
		t.Helper()
		select {
		case err := <-outcomes:
			if (err != nil) != wantFailure {
				t.Fatal("unexpected reload outcome", err)
			}
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("reload timed out")
			return nil
		}
	}
	replace := func(body string) {
		t.Helper()
		tmp := path + ".new"
		if err := os.WriteFile(tmp, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	await(false)
	replace(minimal + "poll_interval: 'SECRET'\n")
	await(true)
	replace(minimal)
	await(false)
	if commits != 0 {
		t.Fatal("identical recovery reached commit", commits)
	}
	if status := m.Status(); status.Generation != 1 || !status.Healthy() || status.LastReloadAt.IsZero() {
		t.Fatal("identical recovery did not clear reload error", status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher leaked")
	}
}

func TestWatchDebouncesBurstsOfWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	applied := make(chan Config, 64)
	go func() {
		_ = Watch(ctx, path, func(c Config) error { applied <- c; return nil }, func(error) {})
	}()
	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("initial load timed out")
	}
	select {
	case c := <-applied:
		t.Fatal("idle watcher reloaded without a file event", c.PollInterval)
	case <-time.After(debounceInterval * 3):
	}
	for i := range 20 {
		body := minimal + "poll_interval: '" + string(rune('1'+i%9)) + "s'\n"
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("burst never applied")
	}
	// The burst finished long before the debounce window elapsed, so it must
	// have collapsed into a single reload, and idle time must stay quiet.
	select {
	case c := <-applied:
		t.Fatal("burst was not debounced", c.PollInterval)
	case <-time.After(debounceInterval * 4):
	}
}

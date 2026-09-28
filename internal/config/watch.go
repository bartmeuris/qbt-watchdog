package config

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// debounceInterval collapses the burst of events an editor or an atomic
	// replace produces into a single reload.
	debounceInterval = 150 * time.Millisecond
	// reconcileInterval re-reads the file even without an event. It covers
	// dropped kernel events and rotations of the password or CA files,
	// which are not in the watched directory.
	reconcileInterval = 5 * time.Second
)

// Watch reloads path whenever it changes, until ctx is cancelled.
//
// The containing directory is watched rather than the file itself, because the
// two ways configuration is delivered in practice both replace the inode:
// an atomic write renames a temporary file over the target, and Kubernetes
// swaps the ..data symlink of a projected volume. A watch on the file would
// follow the old inode and go silent after the first change.
//
// apply must commit atomically or leave the active configuration untouched;
// report receives the outcome of every attempt, nil included, so a caller can
// clear a previously reported failure.
func Watch(ctx context.Context, path string, apply func(Config) error, report func(error)) error {
	return watch(ctx, path, Load, apply, report)
}

func watch(ctx context.Context, path string, load func(string) (Config, error), apply func(Config) error, report func(error)) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return errors.New("cannot resolve configuration location")
	}
	dotenvPath := filepath.Join(filepath.Dir(path), ".env")
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return errors.New("cannot create configuration watcher")
	}
	defer w.Close()
	if err = w.Add(filepath.Dir(path)); err != nil {
		return errors.New("cannot watch configuration directory")
	}
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	debounce := time.NewTimer(debounceInterval)
	defer debounce.Stop()
	// The first tick loads the file, so startup and reload share one path.
	var pending <-chan time.Time = debounce.C
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-w.Events:
			if !ok {
				return errors.New("configuration watcher closed")
			}
			// Renames and removals are reported against the old name, so
			// they are accepted regardless of which entry they name.
			name := filepath.Clean(event.Name)
			if name == path || name == dotenvPath || event.Op&(fsnotify.Rename|fsnotify.Remove) != 0 {
				debounce.Reset(debounceInterval)
				pending = debounce.C
			}
		case _, ok := <-w.Errors:
			if !ok {
				return errors.New("configuration watcher closed")
			}
			report(errors.New("configuration watch event lost; periodic reconciliation active"))
		case <-ticker.C:
			if pending == nil {
				debounce.Reset(debounceInterval)
				pending = debounce.C
			}
		case <-pending:
			pending = nil
			c, err := load(path)
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				err = apply(c)
			}
			report(err)
		}
	}
}

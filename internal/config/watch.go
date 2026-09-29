package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// debounceInterval collapses the burst of events an editor or an atomic
	// replace produces into a single reload.
	debounceInterval = 150 * time.Millisecond
)

type dependencyResolver func(string) ([]string, error)

// Watch reloads path whenever it or a referenced dependency changes, until ctx is cancelled.
//
// Parent directories are watched rather than files themselves, because the two
// ways configuration is delivered in practice both replace the inode: an atomic
// write renames a temporary file over the target, and Kubernetes swaps the
// ..data symlink of a projected volume. A watch on the file would follow the old
// inode and go silent after the first change.
//
// apply must commit atomically or leave the active configuration untouched;
// report receives the outcome of every attempt, nil included, so a caller can
// clear a previously reported failure.
func Watch(ctx context.Context, path string, apply func(Config) error, report func(error)) error {
	return watch(ctx, path, Load, apply, report)
}

func watch(ctx context.Context, path string, load func(string) (Config, error), apply func(Config) error, report func(error)) error {
	return watchWithDependencies(ctx, path, load, func(path string) ([]string, error) {
		return configDependencyPaths(path, environmentSnapshot(os.Environ()))
	}, apply, report)
}

func watchWithDependencies(ctx context.Context, path string, load func(string) (Config, error), dependencies dependencyResolver, apply func(Config) error, report func(error)) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return errors.New("cannot resolve configuration location")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return errors.New("cannot create configuration watcher")
	}
	defer w.Close()
	tracked := watchedDependencies{watcher: w}
	activePaths := coreDependencyPaths(path)
	if err = tracked.update(activePaths); err != nil {
		return errors.New("cannot watch configuration directory")
	}
	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()
	var pending <-chan time.Time
	schedule := func() {
		if !debounce.Stop() {
			select {
			case <-debounce.C:
			default:
			}
		}
		debounce.Reset(debounceInterval)
		pending = debounce.C
	}
	// The first debounced attempt loads the file, so startup and reload share one path.
	schedule()
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-w.Events:
			if !ok {
				return errors.New("configuration watcher closed")
			}
			if tracked.matches(event) {
				schedule()
			}
		case _, ok := <-w.Errors:
			if !ok {
				return errors.New("configuration watcher closed")
			}
			report(errors.New("configuration watch event lost; waiting for next file change"))
		case <-pending:
			pending = nil
			err = nil
			dependenciesReady := dependencies == nil
			if dependencies != nil {
				var paths []string
				paths, err = dependencies(path)
				if err == nil {
					err = tracked.update(paths)
					if err == nil {
						activePaths = paths
						dependenciesReady = true
					}
				}
			}
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				var c Config
				c, err = load(path)
				if ctx.Err() != nil {
					return nil
				}
				if err == nil {
					err = apply(c)
				}
			}
			if err != nil && !dependenciesReady {
				_ = tracked.update(activePaths)
			}
			report(err)
		}
	}
}

type watchedDependencies struct {
	watcher       dependencyWatcher
	directories   map[string]struct{}
	relevantPaths map[string]struct{}
	projectedDirs map[string]struct{}
}

type dependencyWatcher interface {
	Add(string) error
	Remove(string) error
}

func coreDependencyPaths(path string) []string {
	return []string{path, filepath.Join(filepath.Dir(path), ".env")}
}

func (w *watchedDependencies) update(paths []string) error {
	nextDirectories := map[string]struct{}{}
	nextRelevantPaths := map[string]struct{}{}
	nextProjectedDirs := map[string]struct{}{}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := addDependency(path, nextDirectories, nextRelevantPaths, nextProjectedDirs); err != nil {
			return err
		}
	}
	addedDirectories := make([]string, 0, len(nextDirectories))
	for dir := range nextDirectories {
		if _, ok := w.directories[dir]; ok {
			continue
		}
		if err := w.watcher.Add(dir); err != nil {
			for _, added := range addedDirectories {
				_ = w.watcher.Remove(added)
			}
			return errors.New("cannot watch configuration dependency directory")
		}
		addedDirectories = append(addedDirectories, dir)
	}
	for dir := range w.directories {
		if _, ok := nextDirectories[dir]; !ok {
			_ = w.watcher.Remove(dir)
		}
	}
	w.directories = nextDirectories
	w.relevantPaths = nextRelevantPaths
	w.projectedDirs = nextProjectedDirs
	return nil
}

func addDependency(path string, directories, relevantPaths, projectedDirs map[string]struct{}) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return errors.New("cannot resolve configuration dependency location")
	}
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	directories[dir] = struct{}{}
	relevantPaths[path] = struct{}{}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		projectedDirs[dir] = struct{}{}
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	target = filepath.Clean(target)
	directories[filepath.Dir(target)] = struct{}{}
	relevantPaths[target] = struct{}{}
	return nil
}

func (w watchedDependencies) matches(event fsnotify.Event) bool {
	if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove|fsnotify.Chmod) == 0 {
		return false
	}
	name, err := filepath.Abs(event.Name)
	if err != nil {
		return false
	}
	name = filepath.Clean(name)
	if _, ok := w.relevantPaths[name]; ok {
		return true
	}
	if filepath.Base(name) != "..data" {
		return false
	}
	_, ok := w.projectedDirs[filepath.Dir(name)]
	return ok
}

package watchdog

import (
	"bytes"
	"errors"
	"reflect"

	"qbt-watchdog/internal/config"
	"qbt-watchdog/internal/store"
)

// Config is the configuration currently in effect, read lock-free from the
// manager so a request path never waits behind a poll.
func (s *Service) Config() config.Config { return s.manager.Current() }

// ConfigManager exposes the single cell that owns the running configuration
// and the reload health. The process watches the file through this manager
// rather than a second one, so the generation, the last reload error and the
// last reload time have exactly one source and can never disagree.
func (s *Service) ConfigManager() *config.Manager { return s.manager }

// resetTimers implements the reload escalation rule, which is deliberately the
// bluntest defensible one: if anything that could change an outcome changed —
// any policy's action or threshold, dry_run, an interval, the exclusions, the
// action cap, or the endpoint and its credentials — then every per-episode
// clock restarts at now and every per-episode notification marker is cleared.
//
// The consequence is the property operators actually care about: lowering a
// threshold, escalating warn to delete, or turning dry_run off can never
// consume time that accrued under the previous settings. The new settings must
// be satisfied in full by an episode observed entirely under them. Clearing
// the partition as well routes the next observation through the ordinary
// partition-change path, so the attempt budget is refreshed exactly as it is
// for any other new decision (see carriedAttempts).
//
// What is deliberately *not* reset is the safety state a reload must never
// erase: the finite attempt budget and a delete request still awaiting
// confirmation. A reload is not a licence to retry something already in
// flight, and this rule only ever delays an action, never hastens one.
func (s *Service) resetTimers() {
	now := s.clock.Now().UTC()
	for hash, e := range s.state.Tracked {
		e.FirstSeen, e.LastSeen = now, now
		e.Policy, e.DryRunNotified = "", false
		s.state.Tracked[hash] = e
	}
}

// Reload routes a candidate through the manager, so a direct caller gets the
// same validation, generation bump and health reporting as the file watcher.
// The watcher itself calls Apply, because it already holds the manager's lock.
func (s *Service) Reload(next config.Config, prepare func(config.Config) (Client, error)) error {
	next.Once = s.manager.Current().Once
	err := s.manager.Reload(next, func(candidate config.Config) error { return s.Apply(candidate, prepare) })
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publish(nil)
	return err
}

// Apply adopts a candidate the manager has accepted. It is serialized with the
// entire poll, including the final GET/POST pair. Preparation must not mutate
// the running client. Restart-only changes reject the whole candidate, never
// just the unsupported field, so the running configuration always stays
// internally consistent.
//
// Apply deliberately records no reload health of its own: the manager owns the
// generation and the last error, and a second copy here could disagree with it.
func (s *Service) Apply(c config.Config, prepare func(config.Config) (Client, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.Once = s.c.Once
	if err := config.RestartRequiredError(s.c, c); err != nil {
		return err
	}
	if c.TagSync.Enabled && s.state.WatchdogTagPrefix != "" && s.state.WatchdogTagPrefix != c.TagSync.Prefix {
		return errors.New("tag_sync prefix mismatch; disable tag_sync to remediate before enabling the new prefix")
	}
	if reflect.DeepEqual(c, s.c) {
		return nil
	}
	client, err := prepare(c.Clone())
	if err != nil {
		return err
	}
	arrClients, err := s.prepareIntegrations(c)
	if err != nil {
		if closer, ok := client.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		return err
	}
	s.adoptIntegrations(c, arrClients)
	if s.c.EndpointKey() != c.EndpointKey() {
		for _, job := range s.state.RecoveryJobs {
			s.finishRecovery(job, "endpoint_changed")
		}
		s.state.Tracked = map[string]store.Episode{}
		s.state.SeedObserved = map[string]bool{}
		s.torrents = nil
		// Rows and the accepted-list metadata are cleared together, so the UI
		// can never show one endpoint's torrents under another's timestamp.
		s.view.LastTorrentListSuccess = nil
		s.view.TorrentDataStale = false
		s.view.PollError = ""
		s.view.PollDiagnostic = nil
		s.view.QBTVersion = ""
		s.view.WebAPIVersion = ""
	}
	if s.c.SafetyKey() != c.SafetyKey() || s.c.Username != c.Username || s.c.Password != c.Password || s.c.TLSCAFile != c.TLSCAFile || s.c.TLSInsecure != c.TLSInsecure || !bytes.Equal(s.c.TLSCAPEM, c.TLSCAPEM) {
		s.resetTimers()
	}
	if closer, ok := s.client.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
	s.client, s.c = client, c.Clone()
	s.state.SafetyKey, s.state.EndpointKey = c.SafetyKey(), c.EndpointKey()
	if len(s.state.History) > c.HistoryLimit {
		s.state.History = append([]store.Event(nil), s.state.History[len(s.state.History)-c.HistoryLimit:]...)
	}
	s.view.LastSuccess, s.view.QBTUp = nil, false
	s.persist()
	s.publish(nil)
	s.log.Info("configuration reloaded; changed safety settings reset policy timers", "event", "config_reloaded")
	select {
	case s.reloaded <- struct{}{}:
	default:
	}
	return nil
}

// ReportReload mirrors the outcome of a reload attempt into the snapshot and
// logs each transition once. The manager has already recorded the outcome, so
// this only republishes it; loggedReloadError exists purely to keep the log
// quiet while a bad file stays bad, and is never read as reload health.
func (s *Service) ReportReload(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	message := ""
	if err != nil {
		message = err.Error()
	}
	if s.loggedReloadError != message {
		s.loggedReloadError = message
		if err != nil {
			s.log.Error("configuration reload rejected; retaining last known good configuration", "event", "config_reload_error", "error", err)
		}
	}
	s.publish(nil)
}

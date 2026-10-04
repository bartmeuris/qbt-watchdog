package config

import "sync"

// SaveResult separates what was persisted from what the running process
// adopted. A save can succeed while the apply fails; the caller must be able to
// report that distinction and know the previous runtime configuration is still
// active.
type SaveResult struct {
	Stamp   string       `json:"stamp,omitempty"`
	Saved   bool         `json:"saved"`
	Applied bool         `json:"applied"`
	Status  Status       `json:"status"`
	Errors  []FieldError `json:"errors,omitempty"`
	Message string       `json:"message,omitempty"`
}

// Service is the write path the settings UI calls. It serializes saves, applies
// the patch to a candidate, validates before writing, and then applies the
// result through the existing manager reload path. It never introduces a second
// reload system: the file watcher and this service both drive Manager.Reload,
// which skips an identical candidate.
type Service struct {
	editor  *Editor
	manager *Manager
	commit  func(Config) error
	mu      sync.Mutex
}

// NewService wires an editor to the manager that owns the running config.
func NewService(editor *Editor, manager *Manager, commit func(Config) error) *Service {
	return &Service{editor: editor, manager: manager, commit: commit}
}

// Read returns the raw document and its stamp.
func (s *Service) Read() ([]byte, string, error) { return s.editor.Read() }

// Settings returns the typed, source-preserving read model.
func (s *Service) Settings() (Settings, error) { return s.editor.Settings() }

// Environment returns environment-variable availability metadata.
func (s *Service) Environment() ([]EnvVar, error) { return s.editor.Environment() }

// SaveRaw validates and writes a full document, then applies it.
func (s *Service) SaveRaw(raw []byte, stamp string) (SaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	newStamp, err := s.editor.SaveRaw(raw, stamp)
	if err != nil {
		return SaveResult{Errors: []FieldError{fieldError(err)}}, err
	}
	return s.apply(newStamp), nil
}

// Patch applies a structured patch to the current document, validates and
// writes it, then applies it. The stamp is rechecked under the save lock.
func (s *Service) Patch(stamp string, patch Patch) (SaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, currentStamp, err := s.editor.Read()
	if err != nil {
		return SaveResult{}, err
	}
	if currentStamp != stamp {
		return SaveResult{}, ErrConflict
	}
	newStamp, err := s.editor.Save(raw, stamp, patch)
	if err != nil {
		return SaveResult{Errors: []FieldError{fieldError(err)}}, err
	}
	return s.apply(newStamp), nil
}

// apply loads the just-written document and publishes it through the manager.
// A load or commit failure is reported as saved-but-not-applied, never as
// success, and names the previous configuration as still active.
func (s *Service) apply(newStamp string) SaveResult {
	next, err := Load(s.manager.Path())
	if err != nil {
		return SaveResult{
			Stamp: newStamp, Saved: true, Applied: false, Status: s.manager.Status(),
			Message: "saved but not applied: " + err.Error() + "; the previous configuration remains active",
		}
	}
	if err := s.manager.Reload(next, s.commit); err != nil {
		return SaveResult{
			Stamp: newStamp, Saved: true, Applied: false, Status: s.manager.Status(),
			Message: "saved but not applied: " + err.Error() + "; the previous configuration remains active",
		}
	}
	return SaveResult{Stamp: newStamp, Saved: true, Applied: true, Status: s.manager.Status()}
}

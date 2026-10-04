package web

import (
	"errors"

	"qbt-watchdog/internal/config"
)

// errSettingsUnavailable reports that the structured settings seam was not
// wired, so the endpoint answers 404 rather than pretending to work.
var errSettingsUnavailable = errors.New("structured settings are not available")

// ConfigSaver reads and writes the raw configuration document. It is the seam
// the settings editor uses to fetch the file and to persist an edited copy,
// reporting the reload health that resulted from the save.
type ConfigSaver interface {
	Read() (raw []byte, stamp string, err error)
	Save(raw []byte, stamp string) (config.SaveResult, error)
}

// SettingsSaver is the structured settings seam. A ConfigSaver that also
// implements it exposes the typed read model, the structured patch save and the
// environment metadata. The raw editor stays available through ConfigSaver.
type SettingsSaver interface {
	Settings() (config.Settings, error)
	Patch(stamp string, patch config.Patch) (config.SaveResult, error)
	Environment() ([]config.EnvVar, error)
}

// ConfigSaverFunc adapts functions to ConfigSaver and SettingsSaver. The fields
// are named ReadFunc and SaveFunc because a struct cannot carry a field and a
// method with the same name; the methods below satisfy the interfaces. The
// settings functions are optional: a nil one makes the structured endpoints
// report that they are unavailable.
type ConfigSaverFunc struct {
	ReadFunc        func() (raw []byte, stamp string, err error)
	SaveFunc        func(raw []byte, stamp string) (config.SaveResult, error)
	SettingsFunc    func() (config.Settings, error)
	PatchFunc       func(stamp string, patch config.Patch) (config.SaveResult, error)
	EnvironmentFunc func() ([]config.EnvVar, error)
}

func (f ConfigSaverFunc) Read() (raw []byte, stamp string, err error) {
	return f.ReadFunc()
}

func (f ConfigSaverFunc) Save(raw []byte, stamp string) (config.SaveResult, error) {
	return f.SaveFunc(raw, stamp)
}

func (f ConfigSaverFunc) Settings() (config.Settings, error) {
	if f.SettingsFunc == nil {
		return config.Settings{}, errSettingsUnavailable
	}
	return f.SettingsFunc()
}

func (f ConfigSaverFunc) Patch(stamp string, patch config.Patch) (config.SaveResult, error) {
	if f.PatchFunc == nil {
		return config.SaveResult{}, errSettingsUnavailable
	}
	return f.PatchFunc(stamp, patch)
}

func (f ConfigSaverFunc) Environment() ([]config.EnvVar, error) {
	if f.EnvironmentFunc == nil {
		return nil, errSettingsUnavailable
	}
	return f.EnvironmentFunc()
}

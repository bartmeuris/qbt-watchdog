package web

import (
	"qbt-watchdog/internal/config"
)

// ConfigSaver reads and writes the raw configuration document. It is the seam
// the settings editor uses to fetch the file and to persist an edited copy,
// reporting the reload health that resulted from the save.
type ConfigSaver interface {
	Read() (raw []byte, stamp string, err error)
	Save(raw []byte, stamp string) (newStamp string, status config.Status, err error)
}

// ConfigSaverFunc adapts two functions to ConfigSaver. The fields are named
// ReadFunc and SaveFunc because a struct cannot carry a field and a method
// with the same name; the Read and Save methods below satisfy the interface.
type ConfigSaverFunc struct {
	ReadFunc func() (raw []byte, stamp string, err error)
	SaveFunc func(raw []byte, stamp string) (newStamp string, status config.Status, err error)
}

func (f ConfigSaverFunc) Read() (raw []byte, stamp string, err error) {
	return f.ReadFunc()
}

func (f ConfigSaverFunc) Save(raw []byte, stamp string) (newStamp string, status config.Status, err error) {
	return f.SaveFunc(raw, stamp)
}

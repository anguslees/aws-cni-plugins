package main

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
)

// Checkpointer can persist data and (hopefully) restore it later
type Checkpointer interface {
	Checkpoint(data interface{}) error
	Restore(into interface{}) error
}

// NullCheckpoint discards data and always returns "not found". For testing only!
type NullCheckpoint struct{}

// Checkpoint implements the Checkpointer interface in the most
// trivial sense, by just discarding data.
func (c NullCheckpoint) Checkpoint(data interface{}) error {
	return nil
}

// Restore implements the Checkpointer interface in the most trivial
// sense, by always returning "not found".
func (c NullCheckpoint) Restore(into interface{}) error {
	return os.ErrNotExist
}

// JSONFile is a checkpointer that writes to a JSON file
type JSONFile struct {
	path string
}

// NewJSONFile creates a new JsonFile
func NewJSONFile(path string) *JSONFile {
	return &JSONFile{path: path}
}

// Checkpoint implements the Checkpointer interface
func (c *JSONFile) Checkpoint(data interface{}) error {
	f, err := ioutil.TempFile(filepath.Dir(c.path), filepath.Base(c.path)+".tmp*")
	if err != nil {
		return err
	}

	if err := json.NewEncoder(f).Encode(&data); err != nil {
		os.Remove(f.Name())
		return err
	}

	if err := f.Sync(); err != nil {
		os.Remove(f.Name())
		return err
	}

	if err := os.Rename(f.Name(), c.path); err != nil {
		os.Remove(f.Name())
		return err
	}

	return nil
}

// Restore implements the Checkpointer interface
func (c *JSONFile) Restore(into interface{}) error {
	f, err := os.Open(c.path)
	if err != nil {
		return err
	}
	defer f.Close()

	return json.NewDecoder(f).Decode(into)
}

package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// JobState is what `executor prepare` records for the later stages and for reconcile.
type JobState struct {
	Instance
	Source    string    `json:"source"` // pool | cold
	StartedAt time.Time `json:"started_at"`
	Failed    bool      `json:"failed"`
	// Network is the Docker network job containers join (set when the job has services).
	Network string `json:"network,omitempty"`
}

func SaveJobState(path string, st *JobState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// CreateJobState writes a new state file and fails if one already exists.
func CreateJobState(path string, st *JobState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	return f.Close()
}

func LoadJobState(path string) (*JobState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	st := &JobState{}
	return st, json.Unmarshal(data, st)
}

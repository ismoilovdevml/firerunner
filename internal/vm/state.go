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

func LoadJobState(path string) (*JobState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	st := &JobState{}
	return st, json.Unmarshal(data, st)
}

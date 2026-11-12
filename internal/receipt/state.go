package receipt

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateFile = "state.json"

// State ties a receipts directory to one database and replication slot, and
// remembers the last position the recorder confirmed to Postgres. It lets the
// recorder notice when it is pointed at a different server, or when the slot
// was moved or recreated behind its back, either of which would leave a gap
// in the receipts.
type State struct {
	SystemID    string `json:"system_id"` // the server's IDENTIFY_SYSTEM ID
	Timeline    int32  `json:"timeline"`
	Database    string `json:"database"`
	Slot        string `json:"slot"`
	Publication string `json:"publication"`
	AgentRole   string `json:"agent_role"`
	Confirmed   LSN    `json:"confirmed_lsn"` // the recorder never confirms past this
	Gaps        []Gap  `json:"gaps,omitempty"`
}

// Gap is a stretch of WAL the recorder never read, for example after its slot
// was lost and created again. Changes made in a gap are missing from the
// receipts, so undo refuses runs that started before a gap ended.
type Gap struct {
	From LSN       `json:"from"`
	To   LSN       `json:"to"`
	Time time.Time `json:"time"`
}

// ErrNoState means the receipts directory has not been set up by undo init.
var ErrNoState = errors.New("no state.json: run undo init first")

// ReadState reads dir/state.json.
func ReadState(dir string) (State, error) {
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return State{}, ErrNoState
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("read %s: %w", stateFile, err)
	}
	return st, nil
}

// SaveState replaces dir/state.json atomically: it writes a temporary file,
// syncs it, renames it over the old one and syncs the directory, so a crash
// leaves either the old state or the new one.
func (s *Store) SaveState(st State) error {
	return writeState(s.dir, st)
}

func writeState(dir string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, stateFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	err = errors.Join(err, f.Sync(), f.Close())
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, stateFile))
	}
	if err == nil {
		err = syncFile(nil, dir)
	}
	if err != nil {
		return fmt.Errorf("save %s: %w", stateFile, err)
	}
	return nil
}

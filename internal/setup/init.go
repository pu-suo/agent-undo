package setup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pu-suo/agent-undo/internal/db"
	"github.com/pu-suo/agent-undo/internal/receipt"
)

// InitResult reports what Init did.
type InitResult struct {
	Tables      []string     // tables now recorded
	CreatedSlot bool         // false if the slot already existed
	Gap         *receipt.Gap // set when a new epoch started after the slot was lost or moved
}

// Init sets up recording: the objects and triggers in the database, then the
// replication slot, then the receipts directory's state.json. Running it again
// is safe and tags tables created since.
//
// The publication must exist before the slot: pgoutput looks publications up
// as they were at the WAL position it is decoding. Until the slot exists and
// is ready, the tag trigger blocks the agent's writes, so nothing goes
// unrecorded.
//
// If the slot was lost, dropped, or moved past the receipts, some changes
// were never recorded. Init refuses to carry on unless newEpoch is set, in
// which case it records the gap so undo can refuse runs it may have cut.
func Init(ctx context.Context, cfg *pgx.ConnConfig, receipts string, s Settings, newEpoch bool) (InitResult, error) {
	var res InitResult
	if err := s.validate(); err != nil {
		return res, err
	}
	server, err := connectAndIdentify(ctx, cfg)
	if err != nil {
		return res, err
	}
	old, err := receipt.ReadState(receipts)
	hasState := err == nil
	if err != nil && !errors.Is(err, receipt.ErrNoState) {
		return res, err
	}
	if hasState && (old.SystemID != server.SystemID || old.Database != server.Database) {
		return res, fmt.Errorf("%s holds receipts from another database (%s on system %s); use another receipts directory",
			receipts, old.Database, old.SystemID)
	}

	conn, err := db.Connect(ctx, cfg)
	if err != nil {
		return res, err
	}
	defer conn.Close(context.Background())
	slot, err := ReadSlot(ctx, conn, s.Slot)
	if err != nil {
		return res, err
	}
	var hasPublication bool
	err = conn.QueryRow(ctx, `select exists (select from pg_publication where pubname = $1)`, s.Publication).Scan(&hasPublication)
	if err != nil {
		return res, err
	}

	lost := slot.Exists && slot.WALStatus == "lost"
	moved := slot.Exists && !lost && hasState && slot.Confirmed > old.Confirmed
	switch {
	case slot.Exists && slot.Database != server.Database:
		return res, fmt.Errorf("replication slot %s belongs to database %s; choose another --slot", s.Slot, slot.Database)
	case slot.Exists && slot.Plugin != "pgoutput":
		return res, fmt.Errorf("replication slot %s uses plugin %s, not pgoutput", s.Slot, slot.Plugin)
	case slot.Exists && !lost && !hasPublication:
		return res, fmt.Errorf("replication slot %s exists but publication %s does not, so the slot can't read it; "+
			"drop the slot with select pg_drop_replication_slot('%s') and run init again", s.Slot, s.Publication, s.Slot)
	case (lost || moved || (!slot.Exists && hasState)) && !newEpoch:
		return res, fmt.Errorf("replication slot %s was lost, dropped or moved, so changes made after %s were never recorded; "+
			"rerun with --new-epoch to start again and mark the gap", s.Slot, old.Confirmed)
	}

	if res.Tables, err = Install(ctx, conn, s); err != nil {
		return res, err
	}

	start := slot.Confirmed
	if !slot.Exists || lost {
		if lost {
			if err := DropSlot(ctx, conn, s.Slot); err != nil {
				return res, err
			}
		}
		slotCtx, cancel := context.WithTimeout(ctx, time.Minute)
		_, start, err = CreateSlot(slotCtx, cfg, s.Slot)
		cancel()
		if err != nil {
			return res, fmt.Errorf("%w (creating a slot waits for running transactions to finish)", err)
		}
		res.CreatedSlot = true
	}

	state := receipt.State{
		SystemID: server.SystemID, Timeline: server.Timeline, Database: server.Database,
		Slot: s.Slot, Publication: s.Publication, AgentRole: s.AgentRole, Confirmed: start,
	}
	if hasState {
		state.Confirmed, state.Gaps = old.Confirmed, old.Gaps
		if res.CreatedSlot || moved {
			res.Gap = &receipt.Gap{From: old.Confirmed, To: start, Time: time.Now().UTC()}
			state.Confirmed, state.Gaps = start, append(state.Gaps, *res.Gap)
		}
	}
	if !hasState || res.Gap != nil || state.Slot != old.Slot || state.Publication != old.Publication || state.AgentRole != old.AgentRole {
		if err := saveState(receipts, state); err != nil {
			return res, err
		}
	}
	return res, nil
}

// saveState writes state.json while holding the receipts directory's lock,
// so it can't race a running recorder.
func saveState(dir string, st receipt.State) error {
	store, err := receipt.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(store.SaveState(st), store.Close())
}

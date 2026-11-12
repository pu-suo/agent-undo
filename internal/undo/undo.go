// Package undo reverses one agent run's changes and never silently overwrites
// a change someone else made after the agent.
//
// For each row the run touched, undo starts from the row as it is now and
// walks the run's changes from newest to oldest. A column the agent set that
// nobody has touched since goes back to its value from before the run. A
// column someone else changed afterwards keeps their value, and undo reports
// it as flagged. A row undo can't safely identify or write is left alone and
// reported as refused.
//
// Preview makes these decisions in a read-only transaction. Apply first seals
// the run (the database then refuses its writes), waits until the recorder
// has every change of it, and makes the same decisions again with the rows
// locked. It writes them in one transaction and reads every row back before
// committing.
package undo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pu-suo/agent-undo/internal/receipt"
)

// Options says which run to undo and how.
type Options struct {
	Receipts string        // the receipts directory
	Run      string        // the run ID
	Wait     time.Duration // apply: how long to wait for the run's open transactions and the recorder (default 30s)
	PlanHash string        // apply: the previewed plan's hash; apply stops if the plan changed
}

// Plan is what undo decided for a run.
type Plan struct {
	Run      string    `json:"run"`
	Refused  string    `json:"refused,omitempty"`  // set when undo won't touch the run at all
	Warnings []string  `json:"warnings,omitempty"` // things to know that don't change the decisions
	Rows     []RowPlan `json:"rows"`               // in the order the run first changed them
	Hash     string    `json:"hash,omitempty"`     // identifies the decisions, so apply can insist on the previewed plan
	Applied  bool      `json:"applied"`
}

// RowPlan is undo's decision for one row.
type RowPlan struct {
	Table   string      `json:"table"`
	Key     string      `json:"key"`
	Action  Action      `json:"action"`
	Write   WriteKind   `json:"write,omitempty"`
	Columns []string    `json:"columns,omitempty"` // the columns of Values, in table order
	Values  receipt.Row `json:"values,omitempty"`
	Flags   []string    `json:"flags,omitempty"`
	Notes   []string    `json:"notes,omitempty"`
	Reason  string      `json:"reason,omitempty"`
}

// Action summarizes what undo does to a row.
type Action string

const (
	Restore Action = "restore" // put back as it was before the run
	Partial Action = "partial" // put back except where someone else changed it since
	Keep    Action = "keep"    // someone else changed it since, so it stays as they left it
	Nothing Action = "nothing" // already as it was before the run
	Refuse  Action = "refuse"  // undo can't safely touch it
)

// NeedsPerson reports whether a person should look at the plan: undo refused
// something or kept someone else's change.
func (p *Plan) NeedsPerson() bool {
	if p.Refused != "" {
		return true
	}
	for _, r := range p.Rows {
		if r.Action == Partial || r.Action == Keep || r.Action == Refuse {
			return true
		}
	}
	return false
}

// ErrPlanChanged means the rows changed between preview and apply.
var ErrPlanChanged = errors.New("the rows changed since the preview; preview again")

const (
	lockTimeout      = "10s" // how long a write waits for someone else's row lock
	statementTimeout = "60s"
	defaultWait      = 30 * time.Second
)

// Preview works out what undo would do, without changing anything.
func Preview(ctx context.Context, conn *pgx.Conn, o Options) (*Plan, error) {
	u, err := load(ctx, conn, o)
	if err != nil {
		return nil, err
	}
	if err := u.readReceipt(ctx, conn); err != nil || u.plan.Refused != "" {
		return u.plan, err
	}
	if err := u.warnIfBehind(ctx, conn); err != nil {
		return nil, err
	}
	err = pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error { return u.planAll(ctx, tx, false) })
	return u.plan, err
}

// Apply undoes the run. With o.PlanHash set, it writes only if its decisions
// match that previewed plan.
func Apply(ctx context.Context, conn *pgx.Conn, o Options) (*Plan, error) {
	u, err := load(ctx, conn, o)
	if err != nil {
		return nil, err
	}
	var role string
	if err := conn.QueryRow(ctx, `select session_user`).Scan(&role); err != nil {
		return nil, err
	}
	if role == u.state.AgentRole {
		return nil, fmt.Errorf("undo can't run as the agent role %s: its writes would be tagged as a new agent run", role)
	}
	// Refuse early if the receipt already shows why.
	err = u.readReceipt(ctx, conn)
	if errors.Is(err, receipt.ErrNotFound) {
		err = nil // fine for now: the run's first transaction may still be open
	}
	if err != nil || u.plan.Refused != "" {
		return u.plan, err
	}
	if err := u.seal(ctx, conn); err != nil {
		return nil, err
	}
	// Now the run can't write anything more, and the receipt holds all of it.
	if err := u.readReceipt(ctx, conn); err != nil || u.plan.Refused != "" {
		return u.plan, err
	}

	// The first attempt writes in batches without savepoints, which is
	// fastest. If a write breaks a constraint, the whole transaction rolls
	// back, and the second attempt wraps each write in a savepoint so it can
	// skip just that row.
	for _, savepoints := range []bool{false, true} {
		err = pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `select set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)`,
				lockTimeout, statementTimeout)
			if err != nil {
				return err
			}
			// Deferred constraints would only fail at COMMIT, too late to skip a row.
			if _, err := tx.Exec(ctx, `set constraints all immediate`); err != nil {
				return err
			}
			if err := u.planAll(ctx, tx, true); err != nil || u.plan.Refused != "" {
				return err
			}
			if o.PlanHash != "" && o.PlanHash != u.plan.Hash {
				return ErrPlanChanged
			}
			ex := &executor{tx: tx, savepoints: savepoints}
			if err := ex.run(ctx, u.decisions, u.ranks); err != nil {
				return err
			}
			return verify(ctx, tx, u.decisions)
		})
		if !errors.Is(err, errConflict) {
			break
		}
	}
	if err != nil {
		return u.plan, err
	}
	u.report() // include writes that were held back
	u.plan.Applied = u.plan.Refused == ""
	return u.plan, nil
}

// undoer carries one preview or apply.
type undoer struct {
	o         Options
	state     receipt.State
	txns      []receipt.Transaction
	plan      *Plan
	decisions []*decision
	ranks     map[uint32]int
}

// load reads the receipts directory's state and checks that it belongs to
// this database.
func load(ctx context.Context, conn *pgx.Conn, o Options) (*undoer, error) {
	u := &undoer{o: o, plan: &Plan{Run: o.Run}}
	var err error
	if u.state, err = receipt.ReadState(o.Receipts); err != nil {
		return nil, err
	}
	var system, database string
	err = conn.QueryRow(ctx, `select system_identifier::text, current_database() from pg_control_system()`).Scan(&system, &database)
	if err != nil {
		return nil, err
	}
	if system != u.state.SystemID || database != u.state.Database {
		return nil, fmt.Errorf("the receipts in %s were recorded from database %s on system %s, not %s on %s",
			o.Receipts, u.state.Database, u.state.SystemID, database, system)
	}
	return u, nil
}

// readReceipt reads the run's receipt and refuses it if it can't be trusted.
func (u *undoer) readReceipt(ctx context.Context, conn *pgx.Conn) error {
	var err error
	if u.txns, err = receipt.Read(u.o.Receipts, u.o.Run); err != nil {
		return err
	}
	// Where the run started, as the tag trigger saw it. The receipt alone
	// can't tell: its first transactions may be the ones a gap lost.
	var first *receipt.LSN
	var text string
	err = conn.QueryRow(ctx, `select first_lsn::text from agent_undo.run where run = $1`, u.o.Run).Scan(&text)
	switch {
	case err == nil:
		lsn, err := receipt.ParseLSN(text)
		if err != nil {
			return err
		}
		first = &lsn
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("read where run %s started: %w", u.o.Run, err)
	}
	u.plan.Refused = refusal(u.txns, u.state.Gaps, first)
	return nil
}

// refusal explains why a receipt can't be trusted, if it can't. first is
// where the run started, if known.
func refusal(txns []receipt.Transaction, gaps []receipt.Gap, first *receipt.LSN) string {
	for _, t := range txns {
		if t.Commit.Incomplete != "" {
			return fmt.Sprintf("transaction %d may be missing changes: %s", t.Commit.XID, t.Commit.Incomplete)
		}
		for _, c := range t.Changes {
			if c.Op == receipt.Truncate {
				return fmt.Sprintf("the run emptied %s.%s with TRUNCATE, and the WAL does not keep the removed rows", c.Schema, c.Table)
			}
		}
	}
	if first == nil && len(txns) > 0 {
		first = &txns[0].Commit.CommitLSN
	}
	for _, g := range gaps {
		if first == nil || *first < g.To {
			return fmt.Sprintf("recording stopped between %s and %s, after this run started, so changes may be missing", g.From, g.To)
		}
	}
	return ""
}

// seal stops the run from writing, then waits until the recorder has
// everything up to that moment. Sealing updates the run's row, which every
// one of its statements share-locks, so it also waits for the run's open
// transactions to end.
func (u *undoer) seal(ctx context.Context, conn *pgx.Conn) error {
	wait := u.o.Wait
	if wait == 0 {
		wait = defaultWait
	}
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `select set_config('lock_timeout', $1, true)`, strconv.FormatInt(wait.Milliseconds(), 10)+"ms")
		if err == nil {
			_, err = tx.Exec(ctx, `insert into agent_undo.run (run, first_lsn, sealed) values ($1, pg_current_wal_insert_lsn(), true)
				on conflict (run) do update set sealed = true`, u.o.Run)
		}
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" { // lock_not_available
		return fmt.Errorf("run %s still had an open transaction after %s; stop the agent, then try again", u.o.Run, wait)
	}
	if err != nil {
		return fmt.Errorf("seal run %s: %w", u.o.Run, err)
	}

	var now string
	if err := conn.QueryRow(ctx, `select pg_current_wal_insert_lsn()::text`).Scan(&now); err != nil {
		return err
	}
	target, err := receipt.ParseLSN(now)
	if err != nil {
		return err
	}
	err = waitFor(ctx, wait, func() (bool, error) { return u.caughtUp(ctx, conn, target) })
	if err != nil {
		return fmt.Errorf("the recorder has not caught up (is undo record running?): %w", err)
	}
	// The slot caught up, but are these the receipts it wrote? The recorder
	// saves state.json before it confirms, so a current copy has caught up too.
	st, err := receipt.ReadState(u.o.Receipts)
	if err != nil {
		return err
	}
	if st.Confirmed < target {
		return fmt.Errorf("the receipts in %s end at %s, before %s: is undo record writing to this directory?",
			u.o.Receipts, st.Confirmed, target)
	}
	return nil
}

// caughtUp reports whether the recorder has confirmed everything up to target.
func (u *undoer) caughtUp(ctx context.Context, conn *pgx.Conn, target receipt.LSN) (bool, error) {
	var confirmed *string
	err := conn.QueryRow(ctx, `select confirmed_flush_lsn::text from pg_replication_slots where slot_name = $1`,
		u.state.Slot).Scan(&confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil || confirmed == nil {
		return false, err
	}
	lsn, err := receipt.ParseLSN(*confirmed)
	return lsn >= target, err
}

// warnIfBehind adds a warning when the recorder hasn't caught up within a
// couple of seconds, which means the receipt may be missing the newest changes.
func (u *undoer) warnIfBehind(ctx context.Context, conn *pgx.Conn) error {
	var now string
	if err := conn.QueryRow(ctx, `select pg_current_wal_insert_lsn()::text`).Scan(&now); err != nil {
		return err
	}
	target, err := receipt.ParseLSN(now)
	if err != nil {
		return err
	}
	if waitFor(ctx, 2*time.Second, func() (bool, error) { return u.caughtUp(ctx, conn, target) }) != nil {
		u.plan.Warnings = append(u.plan.Warnings,
			"the recorder has not caught up (is undo record running?), so this receipt may be missing the newest changes")
	}
	return ctx.Err()
}

func waitFor(ctx context.Context, limit time.Duration, done func() (bool, error)) error {
	deadline := time.Now().Add(limit)
	for {
		ok, err := done()
		if err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s", limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// planAll loads the tables, groups the receipt by row, reads each live row
// (locking it when apply will write) and decides what to do with it.
func (u *undoer) planAll(ctx context.Context, tx pgx.Tx, lock bool) error {
	var relids []uint32
	for _, t := range u.txns {
		for _, c := range t.Changes {
			if !slices.Contains(relids, c.RelID) {
				relids = append(relids, c.RelID)
			}
		}
	}
	tables, err := loadTables(ctx, tx, relids)
	if err != nil {
		return err
	}
	for _, id := range relids {
		t := tables[id]
		switch {
		case t == nil:
		case len(t.hooks) > 0:
			u.plan.Refused = fmt.Sprintf("%s has %s, which would run again during undo and change rows the receipt restores",
				t.display, t.hooks[0])
		case t.rls:
			u.plan.Refused = fmt.Sprintf("row-level security on %s would hide rows from undo; run it as a role that bypasses it", t.display)
		}
		if u.plan.Refused != "" {
			return nil
		}
	}

	u.ranks = rank(tables)
	hs := group(u.txns, tables)
	var toRead []*history
	for _, h := range hs {
		if h.refused == "" {
			toRead = append(toRead, h)
		}
	}
	rows, err := readRows(ctx, tx, toRead, lock)
	if err != nil {
		return err
	}
	live := map[*history]receipt.Row{}
	for i, h := range toRead {
		live[h] = rows[i]
	}
	u.decisions = nil
	for _, h := range hs {
		if h.refused != "" {
			u.decisions = append(u.decisions, &decision{h: h})
			continue
		}
		u.decisions = append(u.decisions, planRow(h, live[h], live[h] != nil))
	}
	if err := holdDependents(ctx, tx, u.decisions, tables); err != nil {
		return err
	}
	u.report()
	b, _ := json.Marshal(u.plan.Rows)
	sum := sha256.Sum256(b)
	u.plan.Hash = hex.EncodeToString(sum[:6])
	return nil
}

// report turns the decisions into the plan's rows, in receipt order.
func (u *undoer) report() {
	u.plan.Rows = nil
	for _, d := range u.decisions {
		h := d.h
		if h.refused != "" {
			u.plan.Rows = append(u.plan.Rows, RowPlan{Table: h.display, Key: h.keyText, Action: Refuse, Reason: h.refused})
			continue
		}
		r := RowPlan{Table: h.display, Key: h.keyText, Write: d.write, Values: d.set,
			Flags: d.target.flags, Notes: d.target.notes}
		if d.set != nil {
			r.Columns = columnsOf(h.table, d.set)
		}
		switch {
		case d.held != "":
			r.Action, r.Reason, r.Write, r.Columns, r.Values = Keep, d.held, NoWrite, nil, nil
		case d.write != NoWrite && len(r.Flags) > 0:
			r.Action = Partial
		case d.write != NoWrite:
			r.Action = Restore
		case len(r.Flags) > 0:
			r.Action = Keep
		default:
			r.Action = Nothing
		}
		u.plan.Rows = append(u.plan.Rows, r)
	}
}

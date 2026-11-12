package undo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pu-suo/agent-undo/internal/receipt"
)

// WriteKind is the statement undo makes for a row.
type WriteKind string

const (
	NoWrite   WriteKind = ""
	InsertRow WriteKind = "insert" // put back a row the agent deleted
	UpdateRow WriteKind = "update" // put back columns the agent changed
	DeleteRow WriteKind = "delete" // remove a row the agent inserted
)

// batchSize is how many reads or writes go to Postgres in one round trip.
const batchSize = 500

// decision is undo's plan for one row.
type decision struct {
	h      *history
	live   receipt.Row
	target target
	write  WriteKind
	set    receipt.Row // insert: the whole row; update: only the changed columns
	held   string      // why a planned write is held back
}

// planRow compares a row's target with the live row and picks the write.
func planRow(h *history, live receipt.Row, exists bool) *decision {
	t := h.table
	d := &decision{h: h, live: live}
	if exists {
		// The key matched with the column's own equality, which can be looser
		// than exact text (case-insensitive collations, for example).
		if liveKey, _ := pick(live, t); encode(liveKey) != encode(h.keyVals) {
			h.refused = "the row found by this key has a key that prints differently, so it may be a different row"
			return d
		}
	}
	d.target = simulate(t, h.changes, live, exists)
	switch {
	case !exists && d.target.exists:
		d.write, d.set = InsertRow, d.target.row
	case exists && !d.target.exists:
		d.write = DeleteRow
	case exists && d.target.exists:
		set := receipt.Row{}
		for col, v := range d.target.row {
			if !receipt.Same(v, live[col]) {
				set[col] = v
			}
		}
		if len(set) > 0 {
			d.write, d.set = UpdateRow, set
		}
	}
	if d.write == UpdateRow {
		// Changing a referenced column would cascade to other people's rows.
		for _, fk := range t.referencedBy {
			for _, col := range fk.parentCols {
				if _, ok := d.set[col]; ok && changesOthers(fk.onUpdate) {
					h.refused = fmt.Sprintf("restoring %s would change rows in %s through a foreign key", col, fk.childDisplay)
					d.write, d.set = NoWrite, nil
					return d
				}
			}
		}
	}
	return d
}

// holdDependents holds back deletes that would change other people's rows
// through a foreign key that cascades or sets values. Rows the plan deletes
// itself don't count, so an agent's parent and children both go. Holding one
// delete can make another unsafe, so it repeats until nothing changes.
func holdDependents(ctx context.Context, tx pgx.Tx, ds []*decision, tables map[uint32]*table) error {
	for changed := true; changed; {
		changed = false
		deleting := map[rowID]bool{}
		for _, d := range ds {
			if d.write == DeleteRow {
				deleting[rowID{d.h.table.relid, encode(d.h.keyVals)}] = true
			}
		}
		for _, d := range ds {
			if d.write != DeleteRow {
				continue
			}
			reason, err := dependents(ctx, tx, d, tables, deleting)
			if err != nil {
				return err
			}
			if reason != "" {
				d.write, d.held, changed = NoWrite, reason, true
			}
		}
	}
	return nil
}

// dependents explains why deleting d's row would change rows the plan
// doesn't delete, or returns "".
func dependents(ctx context.Context, tx pgx.Tx, d *decision, tables map[uint32]*table, deleting map[rowID]bool) (string, error) {
	for _, fk := range d.h.table.referencedBy {
		if !changesOthers(fk.onDelete) {
			continue // a delete that anything still refers to simply fails
		}
		if fk.childRLS {
			return "row-level security hides rows in " + fk.childDisplay + " that deleting it would change", nil
		}
		vals := make([]*string, len(fk.parentCols))
		for i, col := range fk.parentCols {
			vals[i] = d.live[col]
		}
		child := tables[fk.child]
		keyed := child != nil && len(child.key) > 0
		sql := "select 1 from " + fk.childIdent + " where " + keyCond(fk.childCols, 1) + " limit 1"
		if keyed {
			// Fetch the referring rows' keys, to tell the plan's own deletes apart.
			sql = "select " + strings.Join(quoteAll(child.key), ", ") + " from " + fk.childIdent + " where " + keyCond(fk.childCols, 1)
		}
		res := tx.Conn().PgConn().ExecParams(ctx, sql, args(vals), nil, nil, nil).Read()
		if res.Err != nil {
			return "", res.Err
		}
		for _, r := range res.Rows {
			if !keyed || !deleting[rowID{fk.child, encode(textRow(r))}] {
				return "rows in " + fk.childDisplay + " refer to it, and deleting it would change them", nil
			}
		}
	}
	return "", nil
}

func textRow(cells [][]byte) []*string {
	out := make([]*string, len(cells))
	for i, c := range cells {
		if c != nil {
			s := string(c)
			out[i] = &s
		}
	}
	return out
}

// readRows reads each history's live row, as text, the way pgoutput prints
// values. With lock set, the rows stay locked until the transaction ends.
// Reads go in batches, one round trip per batchSize rows.
func readRows(ctx context.Context, tx pgx.Tx, hs []*history, lock bool) ([]receipt.Row, error) {
	rows := make([]receipt.Row, len(hs))
	for first := 0; first < len(hs); first += batchSize {
		chunk := hs[first:min(first+batchSize, len(hs))]
		batch := &pgconn.Batch{}
		for _, h := range chunk {
			t := h.table
			sql := "select " + strings.Join(quoteAll(t.columns), ", ") + " from " + t.ident + " where " + keyCond(t.key, 1)
			if lock {
				sql += " for update"
			}
			// No formats: values go in and come back as text, parsed and
			// printed by Postgres itself, never converted in Go.
			batch.ExecParams(sql, args(h.keyVals), nil, nil, nil)
		}
		results, err := tx.Conn().PgConn().ExecBatch(ctx, batch).ReadAll()
		if err != nil {
			return nil, fmt.Errorf("read live rows: %w", err)
		}
		for i, res := range results {
			if len(res.Rows) == 0 {
				continue // the row doesn't exist now
			}
			row := receipt.Row{}
			for j, v := range textRow(res.Rows[0]) {
				row[chunk[i].table.columns[j]] = v
			}
			rows[first+i] = row
		}
	}
	return rows, nil
}

// errConflict means a write broke a constraint while running without
// savepoints, which aborts the transaction; apply retries with savepoints.
var errConflict = errors.New("a write broke a constraint")

// errNotOneRow means a write changed no row or several.
var errNotOneRow = errors.New("the write did not change exactly one row")

// executor makes the planned writes in one transaction.
type executor struct {
	tx         pgx.Tx
	savepoints bool // wrap each write so a constraint error skips only that row
}

// run writes the rows in an order that suits foreign keys: deletes from
// child tables first, then updates, then inserts into parent tables first.
// Without savepoints, all writes go in batches and any constraint error
// returns errConflict. With savepoints, a write that breaks a constraint waits
// and is retried after the others; writes that never succeed are held back.
func (e *executor) run(ctx context.Context, ds []*decision, ranks map[uint32]int) error {
	writes := ordered(ds, ranks)
	if !e.savepoints {
		return e.batch(ctx, writes)
	}
	pending := writes
	for len(pending) > 0 {
		var later []*decision
		for _, d := range pending {
			err := e.writeOne(ctx, d)
			if err == nil {
				d.held = ""
				continue
			}
			if !conflict(err) {
				return err
			}
			d.held = "could not write it back: " + err.Error()
			later = append(later, d)
		}
		if len(later) == len(pending) {
			break // no progress: the rest stay as they are
		}
		pending = later
	}
	for _, d := range pending {
		d.write = NoWrite
	}
	return nil
}

func ordered(ds []*decision, ranks map[uint32]int) []*decision {
	phase := map[WriteKind]int{DeleteRow: 0, UpdateRow: 1, InsertRow: 2}
	var writes []*decision
	for _, d := range ds {
		if d.write != NoWrite {
			writes = append(writes, d)
		}
	}
	slices.SortStableFunc(writes, func(a, b *decision) int {
		if a.write != b.write {
			return phase[a.write] - phase[b.write]
		}
		ra, rb := ranks[a.h.table.relid], ranks[b.h.table.relid]
		switch {
		case ra != rb && a.write == DeleteRow:
			return rb - ra // children first
		case ra != rb:
			return ra - rb // parents first
		}
		return b.h.last - a.h.last // newest first
	})
	return writes
}

func (e *executor) batch(ctx context.Context, writes []*decision) error {
	for first := 0; first < len(writes); first += batchSize {
		batch := &pgconn.Batch{}
		for _, d := range writes[first:min(first+batchSize, len(writes))] {
			sql, values := statement(d)
			batch.ExecParams(sql, values, nil, nil, nil)
		}
		results, err := e.tx.Conn().PgConn().ExecBatch(ctx, batch).ReadAll()
		if err == nil {
			for _, res := range results {
				if res.CommandTag.RowsAffected() != 1 {
					err = errNotOneRow
				}
			}
		}
		if conflict(err) {
			return fmt.Errorf("%w: %w", errConflict, err)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *executor) writeOne(ctx context.Context, d *decision) error {
	sql, values := statement(d)
	conn := e.tx.Conn().PgConn()
	if _, err := conn.Exec(ctx, "savepoint undo_row").ReadAll(); err != nil {
		return err
	}
	res := conn.ExecParams(ctx, sql, values, nil, nil, nil).Read()
	err := res.Err
	if err == nil && res.CommandTag.RowsAffected() != 1 {
		err = errNotOneRow
	}
	undo := "release savepoint undo_row"
	if err != nil {
		undo = "rollback to savepoint undo_row"
	}
	if _, spErr := conn.Exec(ctx, undo).ReadAll(); spErr != nil {
		return spErr
	}
	return err
}

// conflict reports whether a write failed because of the data, such as a
// unique or foreign key constraint, rather than a broken connection.
func conflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.Is(err, errNotOneRow) || (errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "23"))
}

// statement builds the SQL for a planned write. Names are quoted, and values
// are text parameters with no declared type, so Postgres parses each one
// with its column's own input function.
func statement(d *decision) (string, [][]byte) {
	t := d.h.table
	cols := columnsOf(t, d.set)
	values := make([]*string, len(cols))
	for i, col := range cols {
		values[i] = d.set[col]
	}
	switch d.write {
	case InsertRow:
		overriding := ""
		if t.identityAlways {
			overriding = " overriding system value"
		}
		return "insert into " + t.ident + " (" + strings.Join(quoteAll(cols), ", ") + ")" + overriding +
			" values (" + placeholders(1, len(cols)) + ")", args(values)
	case UpdateRow:
		set := make([]string, len(cols))
		for i, col := range cols {
			set[i] = pgx.Identifier{col}.Sanitize() + " = $" + strconv.Itoa(i+1)
		}
		return "update " + t.ident + " set " + strings.Join(set, ", ") +
			" where " + keyCond(t.key, len(cols)+1), args(append(values, d.h.keyVals...))
	case DeleteRow:
		return "delete from " + t.ident + " where " + keyCond(t.key, 1), args(d.h.keyVals)
	}
	panic("undo: statement called for a row with no write")
}

// verify reads every written row back and checks it is exactly the target.
// Anything else (a rule, a trigger, a concurrent writer) aborts the apply.
func verify(ctx context.Context, tx pgx.Tx, ds []*decision) error {
	var written []*decision
	var hs []*history
	for _, d := range ds {
		if d.write != NoWrite {
			written = append(written, d)
			hs = append(hs, d.h)
		}
	}
	rows, err := readRows(ctx, tx, hs, false)
	if err != nil {
		return err
	}
	for i, d := range written {
		t := d.h.table
		if (rows[i] != nil) != d.target.exists || (rows[i] != nil && firstDifference(t, rows[i], d.target.row) != "") {
			return fmt.Errorf("after the writes, %s %s is not what undo wrote; nothing was changed", t.display, d.h.keyText)
		}
	}
	return nil
}

// columnsOf returns the row's columns in table order.
func columnsOf(t *table, row receipt.Row) []string {
	var cols []string
	for _, col := range t.columns {
		if _, ok := row[col]; ok {
			cols = append(cols, col)
		}
	}
	return cols
}

func quoteAll(cols []string) []string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = pgx.Identifier{c}.Sanitize()
	}
	return quoted
}

func keyCond(cols []string, first int) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = pgx.Identifier{c}.Sanitize() + " = $" + strconv.Itoa(first+i)
	}
	return strings.Join(parts, " and ")
}

func placeholders(first, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "$" + strconv.Itoa(first+i)
	}
	return strings.Join(parts, ", ")
}

// args turns values into text parameters; nil means NULL.
func args(vals []*string) [][]byte {
	out := make([][]byte, len(vals))
	for i, v := range vals {
		if v != nil {
			out[i] = []byte(*v)
		}
	}
	return out
}

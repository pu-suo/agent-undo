package capture

import (
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pglogrepl"

	"github.com/pu-suo/agent-undo/internal/receipt"
)

// sink stores an agent run's changes. *receipt.Store implements it.
type sink interface {
	LastEnd(run string) (receipt.LSN, error)
	Append(run string, c *receipt.Change) error
	Commit(run string, c *receipt.Commit) error
}

// decoder turns pgoutput messages into receipt changes. It holds the state of
// the one transaction being received and does no I/O except through its sink,
// so tests can drive it with hand-made messages.
//
// Attribution works statement by statement. The tag trigger writes a row to
// agent_undo.tag before each of the agent's statements, so in the stream a
// tag row comes before the changes it covers. Every change after a tag
// belongs to the run the tag names; changes before any tag are someone
// else's and are dropped.
type decoder struct {
	sink      sink
	relations map[uint32]*pglogrepl.RelationMessageV2
	txn       *txn // nil between transactions
}

// txn is the transaction being received.
type txn struct {
	xid       uint32
	commitLSN receipt.LSN // known from BEGIN
	run       string      // named by the latest tag; "" before the first tag
	dropped   int         // changes seen before the first tag
	runs      map[string]*runTxn
	order     []string // runs in the order their first tag arrived
}

// runTxn is one run's part of the transaction.
type runTxn struct {
	skip       bool // already in the receipt: Postgres sent it again
	changes    int
	incomplete string
}

func newDecoder(s sink) *decoder {
	return &decoder{sink: s, relations: map[uint32]*pglogrepl.RelationMessageV2{}}
}

// handle processes one message. lsn is the WAL position the message came
// with. When a transaction's COMMIT has been handled, handle returns the
// position just past it; otherwise it returns 0.
func (d *decoder) handle(lsn receipt.LSN, msg pglogrepl.Message) (receipt.LSN, error) {
	switch m := msg.(type) {
	case *pglogrepl.BeginMessage:
		if d.txn != nil {
			return 0, errors.New("BEGIN arrived inside a transaction")
		}
		d.txn = &txn{xid: m.Xid, commitLSN: receipt.LSN(m.FinalLSN), runs: map[string]*runTxn{}}
	case *pglogrepl.RelationMessageV2:
		d.relations[m.RelationID] = m // a table's columns, sent again whenever they change
	case *pglogrepl.InsertMessageV2:
		return 0, d.row(lsn, m.RelationID, receipt.Insert, 0, nil, m.Tuple)
	case *pglogrepl.UpdateMessageV2:
		return 0, d.row(lsn, m.RelationID, receipt.Update, m.OldTupleType, m.OldTuple, m.NewTuple)
	case *pglogrepl.DeleteMessageV2:
		return 0, d.row(lsn, m.RelationID, receipt.Delete, m.OldTupleType, m.OldTuple, nil)
	case *pglogrepl.TruncateMessageV2:
		return 0, d.truncate(lsn, m.RelationIDs)
	case *pglogrepl.CommitMessage:
		return d.commit(m)
	case *pglogrepl.TypeMessageV2, *pglogrepl.OriginMessage, *pglogrepl.LogicalDecodingMessageV2:
		// Not needed: values stay as text, and messages are not used.
	default:
		return 0, fmt.Errorf("unexpected %s message", msg.Type())
	}
	return 0, nil
}

// row handles an insert, update or delete.
func (d *decoder) row(lsn receipt.LSN, relID uint32, op receipt.Op, oldType uint8, oldTuple, newTuple *pglogrepl.TupleData) error {
	t, rel, err := d.lookup(relID)
	if err != nil {
		return err
	}
	if rel.Namespace == "agent_undo" && rel.RelationName == "tag" {
		if op == receipt.Delete {
			return nil // an admin clearing old tags
		}
		return d.tag(rel, newTuple)
	}
	if t.run == "" {
		t.dropped++ // someone else's change
		return nil
	}
	r := t.runs[t.run]
	if r.skip {
		return nil
	}

	c := &receipt.Change{LSN: lsn, Op: op, RelID: relID, Schema: rel.Namespace, Table: rel.RelationName}
	if op != receipt.Insert {
		// 'O' means the whole old row is in the WAL (REPLICA IDENTITY FULL).
		// Anything less can't be undone.
		if oldType != pglogrepl.UpdateMessageTupleTypeOld || rel.ReplicaIdentity != 'f' {
			r.markIncomplete(fmt.Sprintf("%s.%s does not log whole old rows", rel.Namespace, rel.RelationName))
		} else {
			old, missing, err := decodeRow(rel, oldTuple, nil)
			if err != nil {
				return err
			}
			c.Old = old
			r.markIncomplete(missing)
		}
	}
	if op != receipt.Delete {
		// An unchanged large value isn't sent again; the old row has it.
		row, missing, err := decodeRow(rel, newTuple, c.Old)
		if err != nil {
			return err
		}
		c.New = row
		r.markIncomplete(missing)
	}
	r.changes++
	return d.sink.Append(t.run, c)
}

// markIncomplete records why some of the run's changes in this transaction
// can't be undone. The first reason is kept.
func (r *runTxn) markIncomplete(reason string) {
	if r.incomplete == "" {
		r.incomplete = reason
	}
}

// tag switches the transaction to the run named by a tag row.
func (d *decoder) tag(rel *pglogrepl.RelationMessageV2, tuple *pglogrepl.TupleData) error {
	t := d.txn
	row, _, err := decodeRow(rel, tuple, nil)
	if err != nil {
		return err
	}
	run := ""
	if v := row["run"]; v != nil {
		run = *v
	}
	if !receipt.ValidRunID(run) {
		// The trigger checks run IDs, so only tampering gets here.
		return fmt.Errorf("transaction %d has a tag with an invalid run ID %q", t.xid, run)
	}
	t.run = run
	if _, seen := t.runs[run]; seen {
		return nil
	}

	r := &runTxn{}
	last, err := d.sink.LastEnd(run)
	if err != nil {
		return err
	}
	// The receipt already holds every transaction that committed before its
	// last commit ended.
	r.skip = t.commitLSN < last
	if t.dropped > 0 {
		// An agent transaction whose first changes came without a tag: the
		// trigger is missing from a table the agent wrote.
		r.incomplete = fmt.Sprintf("%d changes in this transaction came before its first tag", t.dropped)
	}
	t.runs[run] = r
	t.order = append(t.order, run)
	return nil
}

// truncate records a TRUNCATE by the agent. Undo refuses runs that contain
// one, because the WAL does not keep the removed rows.
func (d *decoder) truncate(lsn receipt.LSN, relIDs []uint32) error {
	t := d.txn
	if t == nil {
		return errors.New("TRUNCATE arrived outside a transaction")
	}
	if t.run == "" || t.runs[t.run].skip {
		return nil
	}
	for _, id := range relIDs {
		_, rel, err := d.lookup(id)
		if err != nil {
			return err
		}
		t.runs[t.run].changes++
		c := &receipt.Change{LSN: lsn, Op: receipt.Truncate, RelID: id, Schema: rel.Namespace, Table: rel.RelationName}
		if err := d.sink.Append(t.run, c); err != nil {
			return err
		}
	}
	return nil
}

// commit writes a commit line to the receipt of every run in the transaction.
func (d *decoder) commit(m *pglogrepl.CommitMessage) (receipt.LSN, error) {
	t := d.txn
	if t == nil {
		return 0, errors.New("COMMIT arrived outside a transaction")
	}
	if receipt.LSN(m.CommitLSN) != t.commitLSN {
		return 0, fmt.Errorf("COMMIT position %s does not match BEGIN's %s", receipt.LSN(m.CommitLSN), t.commitLSN)
	}
	for _, run := range t.order {
		r := t.runs[run]
		if r.skip || (r.changes == 0 && r.incomplete == "") {
			continue // nothing to record, for example a statement that changed no rows
		}
		err := d.sink.Commit(run, &receipt.Commit{
			XID:        t.xid,
			CommitLSN:  t.commitLSN,
			EndLSN:     receipt.LSN(m.TransactionEndLSN),
			Time:       m.CommitTime.UTC().Truncate(time.Microsecond),
			Changes:    r.changes,
			Incomplete: r.incomplete,
		})
		if err != nil {
			return 0, err
		}
	}
	d.txn = nil
	return receipt.LSN(m.TransactionEndLSN), nil
}

func (d *decoder) lookup(relID uint32) (*txn, *pglogrepl.RelationMessageV2, error) {
	if d.txn == nil {
		return nil, nil, errors.New("a change arrived outside a transaction")
	}
	rel, ok := d.relations[relID]
	if !ok {
		return nil, nil, fmt.Errorf("a change arrived for unknown table %d", relID)
	}
	return d.txn, rel, nil
}

// decodeRow turns a tuple into a Row. A value marked unchanged ('u', a large
// value that isn't sent again) is copied from base. If base doesn't have it,
// the value is left out and the returned reason says why.
func decodeRow(rel *pglogrepl.RelationMessageV2, tuple *pglogrepl.TupleData, base receipt.Row) (receipt.Row, string, error) {
	if tuple == nil || len(tuple.Columns) != len(rel.Columns) {
		// Decoding with the wrong layout would put values in wrong columns.
		return nil, "", fmt.Errorf("%s.%s: row does not match its table's columns", rel.Namespace, rel.RelationName)
	}
	row := make(receipt.Row, len(rel.Columns))
	missing := ""
	for i, col := range tuple.Columns {
		name := rel.Columns[i].Name
		switch col.DataType {
		case pglogrepl.TupleDataTypeNull:
			row[name] = nil
		case pglogrepl.TupleDataTypeText:
			if !utf8.Valid(col.Data) {
				return nil, "", fmt.Errorf("%s.%s.%s: value is not valid UTF-8", rel.Namespace, rel.RelationName, name)
			}
			v := string(col.Data)
			row[name] = &v
		case pglogrepl.TupleDataTypeToast:
			if v, ok := base[name]; ok {
				row[name] = v
			} else {
				missing = fmt.Sprintf("%s.%s.%s: unchanged large value is not in the WAL", rel.Namespace, rel.RelationName, name)
			}
		default:
			return nil, "", fmt.Errorf("%s.%s.%s: unexpected value format %q", rel.Namespace, rel.RelationName, name, col.DataType)
		}
	}
	return row, missing, nil
}

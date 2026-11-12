package capture

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"

	"github.com/pu-suo/agent-undo/internal/receipt"
)

// memSink is an in-memory Sink.
type memSink struct {
	changes map[string][]receipt.Change
	commits map[string][]receipt.Commit
	lastEnd map[string]receipt.LSN
}

func newMemSink() *memSink {
	return &memSink{changes: map[string][]receipt.Change{}, commits: map[string][]receipt.Commit{}, lastEnd: map[string]receipt.LSN{}}
}

func (m *memSink) LastEnd(run string) (receipt.LSN, error) { return m.lastEnd[run], nil }
func (m *memSink) Append(run string, c *receipt.Change) error {
	m.changes[run] = append(m.changes[run], *c)
	return nil
}
func (m *memSink) Commit(run string, c *receipt.Commit) error {
	m.commits[run] = append(m.commits[run], *c)
	return nil
}

const (
	customers = 16385
	tagTable  = 16400
)

func relation(id uint32, schema, name string, identity uint8, columns ...string) *pglogrepl.RelationMessageV2 {
	m := &pglogrepl.RelationMessageV2{}
	m.RelationID, m.Namespace, m.RelationName, m.ReplicaIdentity = id, schema, name, identity
	for _, c := range columns {
		m.Columns = append(m.Columns, &pglogrepl.RelationMessageColumn{Name: c})
	}
	return m
}

// tuple builds a row: "~" stands for an unchanged large value, "NULL" for NULL.
func tuple(values ...string) *pglogrepl.TupleData {
	t := &pglogrepl.TupleData{ColumnNum: uint16(len(values))}
	for _, v := range values {
		switch v {
		case "NULL":
			t.Columns = append(t.Columns, &pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeNull})
		case "~":
			t.Columns = append(t.Columns, &pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeToast})
		default:
			t.Columns = append(t.Columns, &pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeText, Data: []byte(v)})
		}
	}
	return t
}

func begin(xid uint32, commit receipt.LSN) *pglogrepl.BeginMessage {
	return &pglogrepl.BeginMessage{Xid: xid, FinalLSN: pglogrepl.LSN(commit), CommitTime: time.Unix(1, 0)}
}

func end(commit receipt.LSN) *pglogrepl.CommitMessage {
	return &pglogrepl.CommitMessage{CommitLSN: pglogrepl.LSN(commit), TransactionEndLSN: pglogrepl.LSN(commit + 0x30), CommitTime: time.Unix(1, 0)}
}

func tag(run string) *pglogrepl.UpdateMessageV2 {
	m := &pglogrepl.UpdateMessageV2{}
	m.RelationID, m.NewTuple = tagTable, tuple("4751", run)
	return m
}

func update(old, new *pglogrepl.TupleData) *pglogrepl.UpdateMessageV2 {
	m := &pglogrepl.UpdateMessageV2{}
	m.RelationID, m.OldTupleType, m.OldTuple, m.NewTuple = customers, pglogrepl.UpdateMessageTupleTypeOld, old, new
	return m
}

func insert(values ...string) *pglogrepl.InsertMessageV2 {
	m := &pglogrepl.InsertMessageV2{}
	m.RelationID, m.Tuple = customers, tuple(values...)
	return m
}

// feed sends messages to a fresh decoder (after the two Relation messages)
// and returns the sink, the last end position and the first error.
func feed(t *testing.T, sink *memSink, msgs ...pglogrepl.Message) (receipt.LSN, error) {
	t.Helper()
	d := newDecoder(sink)
	all := append([]pglogrepl.Message{
		relation(customers, "public", "customers", 'f', "id", "plan", "body"),
		relation(tagTable, "agent_undo", "tag", 'd', "pid", "run"),
	}, msgs...)
	var last receipt.LSN
	for _, m := range all {
		end, err := d.handle(0x100, m)
		if err != nil {
			return last, err
		}
		if end != 0 {
			last = end
		}
	}
	return last, nil
}

func TestTaggedChangeIsRecorded(t *testing.T) {
	sink := newMemSink()
	last, err := feed(t, sink, begin(770, 0x1000), tag("run-42"),
		update(tuple("1", "pro", "big"), tuple("1", "cancelled", "~")), end(0x1000))
	if err != nil {
		t.Fatal(err)
	}
	if last != 0x1030 {
		t.Errorf("handled up to %s, want 0/1030 (the commit's end)", last)
	}
	got := sink.changes["run-42"]
	if len(got) != 1 || *got[0].Old["plan"] != "pro" || *got[0].New["plan"] != "cancelled" {
		t.Fatalf("changes = %+v, want one update from pro to cancelled", got)
	}
	if *got[0].New["body"] != "big" {
		t.Errorf("unchanged large value = %v, want it copied from the old row", got[0].New["body"])
	}
	c := sink.commits["run-42"]
	if len(c) != 1 || c[0].XID != 770 || c[0].EndLSN != 0x1030 || c[0].Changes != 1 || c[0].Incomplete != "" {
		t.Errorf("commits = %+v", c)
	}
}

func TestUntaggedTransactionIsDropped(t *testing.T) {
	sink := newMemSink()
	last, err := feed(t, sink, begin(771, 0x1000), insert("2", "pro", "x"), end(0x1000))
	if err != nil || last != 0x1030 {
		t.Fatalf("feed = %s, %v; want 0/1030 and no error", last, err)
	}
	if len(sink.changes) != 0 || len(sink.commits) != 0 {
		t.Errorf("a person's transaction was recorded: %+v", sink.commits)
	}
}

func TestChangesBeforeTheFirstTagMakeTheRunIncomplete(t *testing.T) {
	sink := newMemSink()
	_, err := feed(t, sink, begin(772, 0x1000), insert("3", "pro", "x"), tag("run-42"),
		insert("4", "pro", "x"), end(0x1000))
	if err != nil {
		t.Fatal(err)
	}
	c := sink.commits["run-42"]
	if len(c) != 1 || !strings.Contains(c[0].Incomplete, "before its first tag") {
		t.Errorf("commits = %+v, want an incomplete commit", c)
	}
}

func TestEachStatementBelongsToItsTag(t *testing.T) {
	sink := newMemSink()
	_, err := feed(t, sink, begin(773, 0x1000), tag("run-1"), insert("5", "a", "x"),
		tag("run-2"), insert("6", "b", "x"), insert("7", "b", "x"), end(0x1000))
	if err != nil {
		t.Fatal(err)
	}
	if len(sink.changes["run-1"]) != 1 || len(sink.changes["run-2"]) != 2 {
		t.Errorf("run-1 got %d changes and run-2 got %d, want 1 and 2", len(sink.changes["run-1"]), len(sink.changes["run-2"]))
	}
	if len(sink.commits["run-1"]) != 1 || len(sink.commits["run-2"]) != 1 {
		t.Errorf("want one commit line in each receipt, got %+v", sink.commits)
	}
}

func TestTransactionsAlreadyInTheReceiptAreSkipped(t *testing.T) {
	sink := newMemSink()
	sink.lastEnd["run-42"] = 0x1030 // the receipt already ends with this transaction
	_, err := feed(t, sink, begin(770, 0x1000), tag("run-42"), insert("1", "a", "x"), end(0x1000),
		begin(774, 0x2000), tag("run-42"), insert("2", "a", "x"), end(0x2000))
	if err != nil {
		t.Fatal(err)
	}
	c := sink.commits["run-42"]
	if len(c) != 1 || c[0].XID != 774 || len(sink.changes["run-42"]) != 1 {
		t.Errorf("commits = %+v, want only the new transaction 774", c)
	}
}

func TestTagWithoutChangesWritesNothing(t *testing.T) {
	sink := newMemSink()
	if _, err := feed(t, sink, begin(775, 0x1000), tag("run-42"), end(0x1000)); err != nil {
		t.Fatal(err)
	}
	if len(sink.commits) != 0 {
		t.Errorf("a statement that changed nothing was recorded: %+v", sink.commits)
	}
}

func TestMissingOldRowMakesTheRunIncomplete(t *testing.T) {
	sink := newMemSink()
	u := update(nil, tuple("1", "cancelled", "x"))
	u.OldTupleType = 0 // the table doesn't use REPLICA IDENTITY FULL
	if _, err := feed(t, sink, begin(776, 0x1000), tag("run-42"), u, end(0x1000)); err != nil {
		t.Fatal(err)
	}
	if c := sink.commits["run-42"]; len(c) != 1 || !strings.Contains(c[0].Incomplete, "whole old rows") {
		t.Errorf("commits = %+v, want an incomplete commit", c)
	}
}

func TestTruncateIsRecorded(t *testing.T) {
	sink := newMemSink()
	tr := &pglogrepl.TruncateMessageV2{}
	tr.RelationIDs = []uint32{customers}
	if _, err := feed(t, sink, begin(777, 0x1000), tag("run-42"), tr, end(0x1000)); err != nil {
		t.Fatal(err)
	}
	if ch := sink.changes["run-42"]; len(ch) != 1 || ch[0].Op != receipt.Truncate || ch[0].Table != "customers" {
		t.Errorf("changes = %+v, want one truncate of customers", ch)
	}
}

func TestProtocolErrors(t *testing.T) {
	short := update(tuple("1", "pro"), tuple("1", "cancelled")) // two values for three columns
	tests := map[string][]pglogrepl.Message{
		"invalid run ID":     {begin(1, 0x1000), tag("psql")},
		"wrong column count": {begin(1, 0x1000), tag("run-42"), short},
		"commit mismatch":    {begin(1, 0x1000), end(0x2000)},
		"change outside":     {insert("1", "a", "x")},
		"binary value": {begin(1, 0x1000), tag("run-42"), &pglogrepl.InsertMessageV2{InsertMessage: pglogrepl.InsertMessage{
			RelationID: customers, Tuple: &pglogrepl.TupleData{Columns: []*pglogrepl.TupleDataColumn{
				{DataType: pglogrepl.TupleDataTypeBinary}, {DataType: 't'}, {DataType: 't'}}}}}},
	}
	for name, msgs := range tests {
		if _, err := feed(t, newMemSink(), msgs...); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

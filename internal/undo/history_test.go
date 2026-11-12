package undo

import (
	"strconv"
	"strings"
	"testing"

	"github.com/pu-suo/agent-undo/internal/receipt"
)

var customers = &table{
	relid: 1, display: "public.customers", key: []string{"id"},
	columns: []string{"id", "plan", "phone"},
	has:     map[string]bool{"id": true, "plan": true, "phone": true},
}

// row builds a row from "column=value" pairs; "column=NULL" is NULL.
func row(pairs ...string) receipt.Row {
	r := receipt.Row{}
	for _, p := range pairs {
		col, val, _ := strings.Cut(p, "=")
		if val == "NULL" {
			r[col] = nil
		} else {
			r[col] = &val
		}
	}
	return r
}

func update(old, new receipt.Row) receipt.Change {
	return receipt.Change{Op: receipt.Update, RelID: 1, Old: old, New: new}
}

func TestSimulate(t *testing.T) {
	pro := row("id=1", "plan=pro", "phone=555")
	cancelled := row("id=1", "plan=cancelled", "phone=555")
	tests := []struct {
		name    string
		changes []receipt.Change
		live    receipt.Row // nil: the row doesn't exist now
		exists  bool        // the target
		want    receipt.Row // the target row
		flags   int
	}{
		{
			name:    "nobody touched it since: restore",
			changes: []receipt.Change{update(pro, cancelled)},
			live:    cancelled, exists: true, want: pro,
		},
		{
			name:    "someone changed another column: restore the agent's column, keep theirs",
			changes: []receipt.Change{update(pro, cancelled)},
			live:    row("id=1", "plan=cancelled", "phone=999"), exists: true,
			want: row("id=1", "plan=pro", "phone=999"),
		},
		{
			name:    "someone changed the same column: keep theirs and flag it",
			changes: []receipt.Change{update(pro, cancelled)},
			live:    row("id=1", "plan=basic", "phone=555"), exists: true,
			want: row("id=1", "plan=basic", "phone=555"), flags: 1,
		},
		{
			name:    "already undone: nothing to do",
			changes: []receipt.Change{update(pro, cancelled)},
			live:    pro, exists: true, want: pro,
		},
		{
			name: "two agent updates in a row: back to the first old value",
			changes: []receipt.Change{
				update(pro, cancelled),
				update(cancelled, row("id=1", "plan=trial", "phone=555")),
			},
			live: row("id=1", "plan=trial", "phone=555"), exists: true, want: pro,
		},
		{
			name: "a person's edit between two agent updates: undo only the later one",
			changes: []receipt.Change{
				update(pro, cancelled),
				update(row("id=1", "plan=basic", "phone=555"), row("id=1", "plan=trial", "phone=555")),
			},
			live: row("id=1", "plan=trial", "phone=555"), exists: true,
			want: row("id=1", "plan=basic", "phone=555"), flags: 1,
		},
		{
			name:    "the agent inserted the row: delete it",
			changes: []receipt.Change{{Op: receipt.Insert, RelID: 1, New: pro}},
			live:    pro, exists: false,
		},
		{
			name:    "someone edited the row the agent inserted: keep it",
			changes: []receipt.Change{{Op: receipt.Insert, RelID: 1, New: pro}},
			live:    row("id=1", "plan=pro", "phone=999"), exists: true,
			want: row("id=1", "plan=pro", "phone=999"), flags: 1,
		},
		{
			name:    "the agent deleted the row: put it back",
			changes: []receipt.Change{{Op: receipt.Delete, RelID: 1, Old: pro}},
			live:    nil, exists: true, want: pro,
		},
		{
			name:    "someone reused the deleted row's key: keep their row",
			changes: []receipt.Change{{Op: receipt.Delete, RelID: 1, Old: pro}},
			live:    row("id=1", "plan=new", "phone=NULL"), exists: true,
			want: row("id=1", "plan=new", "phone=NULL"), flags: 1,
		},
		{
			name:    "someone deleted the row the agent updated: leave it deleted",
			changes: []receipt.Change{update(pro, cancelled)},
			live:    nil, exists: false, flags: 1,
		},
		{
			name: "updated, then deleted by the agent: put back the original",
			changes: []receipt.Change{
				update(pro, cancelled),
				{Op: receipt.Delete, RelID: 1, Old: cancelled},
			},
			live: nil, exists: true, want: pro,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := simulate(customers, tt.changes, tt.live, tt.live != nil)
			if got.exists != tt.exists {
				t.Fatalf("exists = %v, want %v (flags %v)", got.exists, tt.exists, got.flags)
			}
			if tt.exists && firstDifference(customers, got.row, tt.want) != "" {
				t.Errorf("row = %v, want %v", receipt.Quote(got.row["plan"])+" "+receipt.Quote(got.row["phone"]), receipt.Quote(tt.want["plan"])+" "+receipt.Quote(tt.want["phone"]))
			}
			if len(got.flags) != tt.flags {
				t.Errorf("flags = %q, want %d", got.flags, tt.flags)
			}
		})
	}
}

func TestDroppedColumnIsNotedNotWritten(t *testing.T) {
	old := row("id=1", "plan=pro", "phone=555", "fax=123")
	got := simulate(customers, []receipt.Change{{Op: receipt.Delete, RelID: 1, Old: old}}, nil, false)
	if _, ok := got.row["fax"]; ok || len(got.notes) != 1 {
		t.Errorf("row = %v, notes = %q; want fax left out and noted", got.row, got.notes)
	}
}

func TestGroupRefusesKeyChanges(t *testing.T) {
	txns := []receipt.Transaction{{Changes: []receipt.Change{
		update(row("id=1", "plan=pro", "phone=555"), row("id=2", "plan=pro", "phone=555")),
	}}}
	rows := group(txns, map[uint32]*table{1: customers})
	if len(rows) != 2 || rows[0].refused == "" || rows[1].refused == "" {
		t.Errorf("want both the old and the new key refused, got %d rows", len(rows))
	}
}

func TestSameComparesText(t *testing.T) {
	a, b := "1.50", "1.5"
	if receipt.Same(&a, &b) {
		t.Error("1.50 and 1.5 print differently, so undo must treat them as different")
	}
	if !receipt.Same(nil, nil) || receipt.Same(&a, nil) {
		t.Error("NULL handling is wrong")
	}
}

// The agent deleted a row and inserted a new one with the same key; then a
// person deleted it. Putting the original back would undo the person's delete.
func TestRecreatedRowDeletedByAPerson(t *testing.T) {
	changes := []receipt.Change{
		{Op: receipt.Delete, RelID: 1, Old: row("id=1", "plan=pro", "phone=555")},
		{Op: receipt.Insert, RelID: 1, New: row("id=1", "plan=new", "phone=555")},
	}
	got := simulate(customers, changes, nil, false)
	if got.exists || len(got.flags) != 1 {
		t.Errorf("exists = %v, flags = %q; want the row left deleted and flagged", got.exists, got.flags)
	}
}

func TestGroupRefusesUnusableKeys(t *testing.T) {
	tables := map[uint32]*table{1: customers}
	tests := map[string]receipt.Change{
		"key changed":     update(row("id=1", "plan=pro", "phone=555"), row("id=2", "plan=pro", "phone=555")),
		"old key NULL":    update(row("id=NULL", "plan=pro", "phone=555"), row("id=1", "plan=pro", "phone=555")),
		"key not in rows": {Op: receipt.Insert, RelID: 1, New: row("plan=pro")},
		"unknown table":   {Op: receipt.Insert, RelID: 99, New: row("id=1")},
	}
	for name, c := range tests {
		rows := group([]receipt.Transaction{{Changes: []receipt.Change{c}}}, tables)
		if len(rows) == 0 || rows[0].refused == "" {
			t.Errorf("%s: not refused", name)
		}
	}
}

func TestRenamedColumnIsRefused(t *testing.T) {
	renamed := &table{relid: 1, key: []string{"id"}, columns: []string{"id", "tier", "phone"},
		has: map[string]bool{"id": true, "tier": true, "phone": true}}
	txns := []receipt.Transaction{{Changes: []receipt.Change{
		update(row("id=1", "plan=pro", "phone=555"), row("id=1", "plan=cancelled", "phone=555")),
	}}}
	if rows := group(txns, map[uint32]*table{1: renamed}); rows[0].refused == "" {
		t.Error("plan is gone and tier is new: a likely rename must be refused")
	}
	dropped := &table{relid: 1, key: []string{"id"}, columns: []string{"id", "phone"}, has: map[string]bool{"id": true, "phone": true}}
	if rows := group(txns, map[uint32]*table{1: dropped}); rows[0].refused != "" {
		t.Errorf("a column that was only dropped should not be refused: %s", rows[0].refused)
	}
}

func TestRefusal(t *testing.T) {
	txn := func(commit receipt.LSN, incomplete string, ops ...receipt.Op) receipt.Transaction {
		tx := receipt.Transaction{Commit: receipt.Commit{CommitLSN: commit, Incomplete: incomplete}}
		for _, op := range ops {
			tx.Changes = append(tx.Changes, receipt.Change{Op: op, Schema: "public", Table: "t"})
		}
		return tx
	}
	lsn := func(l receipt.LSN) *receipt.LSN { return &l }
	gap := []receipt.Gap{{From: 0x100, To: 0x200}}
	tests := []struct {
		name   string
		txns   []receipt.Transaction
		gaps   []receipt.Gap
		first  *receipt.LSN
		refuse bool
	}{
		{"clean", []receipt.Transaction{txn(0x300, "", receipt.Update)}, nil, nil, false},
		{"incomplete", []receipt.Transaction{txn(0x300, "missing", receipt.Update)}, nil, nil, true},
		{"truncate", []receipt.Transaction{txn(0x300, "", receipt.Truncate)}, nil, nil, true},
		{"started after the gap", []receipt.Transaction{txn(0x300, "", receipt.Update)}, gap, lsn(0x250), false},
		// The run's first recorded commit is after the gap, but it started
		// before: its first changes may be the ones the gap lost.
		{"started before the gap", []receipt.Transaction{txn(0x300, "", receipt.Update)}, gap, lsn(0x150), true},
		{"start unknown, first commit after the gap", []receipt.Transaction{txn(0x300, "", receipt.Update)}, gap, nil, false},
	}
	for _, tt := range tests {
		if got := refusal(tt.txns, tt.gaps, tt.first) != ""; got != tt.refuse {
			t.Errorf("%s: refused = %v, want %v", tt.name, got, tt.refuse)
		}
	}
}

func TestOrderedPutsParentsFirstForInserts(t *testing.T) {
	parent := &table{relid: 1}
	child := &table{relid: 2, parents: []uint32{1}}
	ranks := rank(map[uint32]*table{1: parent, 2: child})
	d := func(tb *table, w WriteKind, last int) *decision {
		return &decision{h: &history{table: tb, last: last}, write: w}
	}
	got := ordered([]*decision{
		d(child, InsertRow, 3), d(parent, InsertRow, 1), d(parent, DeleteRow, 5),
		d(child, DeleteRow, 4), d(parent, UpdateRow, 2),
	}, ranks)
	want := []string{"delete 2", "delete 1", "update 1", "insert 1", "insert 2"}
	for i, w := range want {
		if g := string(got[i].write) + " " + strconv.Itoa(int(got[i].h.table.relid)); g != w {
			t.Errorf("write %d = %s, want %s", i, g, w)
		}
	}
}

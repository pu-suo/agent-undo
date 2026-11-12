package undo

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/pu-suo/agent-undo/internal/receipt"
)

// history is the run's changes to one row, oldest first.
type history struct {
	table   *table // nil if the table is gone or not tagged
	display string // the table's name, for reports
	keyVals []*string
	keyText string // the key for reports, like id=1
	changes []receipt.Change
	last    int    // position of the row's newest change in the receipt
	refused string // why undo won't touch this row, if it won't
}

type rowID struct {
	relid uint32
	key   string
}

// group sorts the run's changes into one history per row, in the order the
// rows first appear in the receipt. Rows undo can't safely identify are
// refused here.
func group(txns []receipt.Transaction, tables map[uint32]*table) []*history {
	byRow := map[rowID]*history{}
	var rows []*history
	get := func(c receipt.Change, t *table, id rowID, keyVals []*string) *history {
		h := byRow[id]
		if h == nil {
			h = &history{table: t, display: c.Schema + "." + c.Table, keyVals: keyVals, keyText: id.key}
			if t != nil {
				h.display = t.display
				if len(keyVals) == len(t.key) {
					h.keyText = keyText(t.key, keyVals)
				}
			}
			byRow[id] = h
			rows = append(rows, h)
		}
		return h
	}

	pos := 0
	for _, txn := range txns {
		for _, c := range txn.Changes {
			pos++
			t := tables[c.RelID]
			image := c.New
			if c.Op == receipt.Delete {
				image = c.Old
			}
			keyVals, missing := pick(image, t)
			if t == nil || len(t.key) == 0 || missing != "" {
				// Without a key, the change can't be matched to a live row.
				h := get(c, t, rowID{c.RelID, "#" + strconv.Itoa(pos)}, nil)
				h.changes, h.last = append(h.changes, c), pos
				switch {
				case t == nil:
					h.refused = "the table is gone, or agent-undo doesn't tag it"
				case len(t.key) == 0:
					h.refused = "the table has no primary key, so the row can't be identified"
				default:
					h.refused = "primary key column " + missing + " is missing or NULL in the receipt"
				}
				continue
			}

			h := get(c, t, rowID{c.RelID, encode(keyVals)}, keyVals)
			h.changes, h.last = append(h.changes, c), pos
			if c.Op != receipt.Update {
				continue
			}
			oldKey, missing := pick(c.Old, t)
			switch {
			case missing != "":
				h.refused = "primary key column " + missing + " is missing or NULL in the row before the agent's change"
			case encode(oldKey) != encode(keyVals):
				// Undo would have to follow the row to its old key. v1 refuses
				// both the old and the new key instead.
				reason := "the agent changed this row's primary key"
				h.refused = reason
				get(c, t, rowID{c.RelID, encode(oldKey)}, oldKey).refused = reason
			}
		}
	}

	for _, h := range rows {
		if h.refused == "" {
			h.refused = renamedColumns(h)
		}
	}
	return rows
}

// renamedColumns explains why a row is refused when the table's columns look
// renamed since the run: some receipt columns are gone and the table has new
// ones. A rename keeps the agent's value under the new name, and undo can't
// tell which column it went to. A column that is only dropped is fine: its
// value is gone in every version of the data.
func renamedColumns(h *history) string {
	t := h.table
	inReceipt := map[string]bool{}
	for _, c := range h.changes {
		for col := range c.Old {
			inReceipt[col] = true
		}
		for col := range c.New {
			inReceipt[col] = true
		}
	}
	var gone, added []string
	for _, col := range slices.Sorted(maps.Keys(inReceipt)) {
		if !t.has[col] {
			gone = append(gone, col)
		}
	}
	for _, col := range t.columns {
		if !inReceipt[col] {
			added = append(added, col)
		}
	}
	if len(gone) == 0 || len(added) == 0 {
		return ""
	}
	return fmt.Sprintf("the table's columns changed since the run (%s gone, %s new); if a column was renamed, undo can't follow it",
		strings.Join(gone, ", "), strings.Join(added, ", "))
}

// pick returns the row's primary key values, or the name of a key column the
// row lacks or has as NULL.
func pick(row receipt.Row, t *table) ([]*string, string) {
	if t == nil {
		return nil, ""
	}
	vals := make([]*string, len(t.key))
	for i, col := range t.key {
		v := row[col]
		if v == nil {
			return nil, col
		}
		vals[i] = v
	}
	return vals, ""
}

func encode(vals []*string) string {
	b, _ := json.Marshal(vals)
	return string(b)
}

// keyText prints a key for people, like id=1. Simple values are shown bare.
func keyText(cols []string, vals []*string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		v := receipt.Quote(vals[i])
		if bare(*vals[i]) {
			v = *vals[i]
		}
		parts[i] = c + "=" + v
	}
	return strings.Join(parts, ", ")
}

// bare reports whether a value reads unambiguously without quotes.
func bare(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return true
}

// target is what a row should become after undo.
type target struct {
	exists bool
	row    receipt.Row // writable columns
	flags  []string    // later changes by someone else, which undo keeps
	notes  []string
}

// simulate works out a row's target. It starts from the row as it is now
// and walks the run's changes from newest to oldest. For each change it asks
// whether the row still shows what the agent left; if so the change is
// reversed, and if not, someone else changed it since, so that part is kept
// and flagged. A column the agent set and nobody touched goes back to its
// value before the run; columns the agent never changed are never written.
func simulate(t *table, changes []receipt.Change, live receipt.Row, exists bool) target {
	tg := target{exists: exists, row: keep(t, live)}
	kept := map[string]bool{} // columns someone else changed after the agent
	for _, col := range droppedColumns(t, changes) {
		tg.notes = append(tg.notes, "column "+col+" no longer exists, so its old value can't be restored")
	}

walk:
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		switch c.Op {
		case receipt.Update:
			if !tg.exists {
				tg.flags = append(tg.flags, "someone deleted the row after the agent changed it")
				break walk
			}
			for _, col := range c.New.Columns() {
				if !t.has[col] || kept[col] || receipt.Same(c.Old[col], c.New[col]) {
					continue // not a column the agent changed here
				}
				switch now := tg.row[col]; {
				case receipt.Same(now, c.New[col]): // still the agent's value
					tg.row[col] = c.Old[col]
				case receipt.Same(now, c.Old[col]): // already back to the old value
				default:
					kept[col] = true
					tg.flags = append(tg.flags, fmt.Sprintf("%s: someone changed it to %s after the agent set it to %s",
						col, receipt.Quote(now), receipt.Quote(c.New[col])))
				}
			}
		case receipt.Insert:
			if !tg.exists {
				if i > 0 {
					// The row existed before the agent re-created it, and
					// someone deleted it since. Putting the old row back would
					// undo their delete.
					tg.flags = append(tg.flags, "someone deleted the row after the agent re-created it")
					break walk
				}
				continue // someone already deleted the row the agent inserted
			}
			if len(kept) > 0 {
				tg.flags = append(tg.flags, "the row the agent inserted was edited since, so it is kept")
				break walk
			}
			if col := firstDifference(t, tg.row, c.New); col != "" {
				tg.flags = append(tg.flags, fmt.Sprintf("someone changed %s in the row the agent inserted, so it is kept", col))
				break walk
			}
			tg.exists, tg.row = false, nil
		case receipt.Delete:
			if !tg.exists {
				tg.exists, tg.row = true, keep(t, c.Old)
				continue
			}
			if firstDifference(t, tg.row, c.Old) != "" {
				tg.flags = append(tg.flags, "someone created a different row with this key after the agent deleted it")
				break walk
			}
			// Someone already put the same row back.
		}
	}
	return tg
}

// keep copies the columns undo may write.
func keep(t *table, row receipt.Row) receipt.Row {
	if row == nil {
		return nil
	}
	out := receipt.Row{}
	for _, col := range t.columns {
		if v, ok := row[col]; ok {
			out[col] = v
		}
	}
	return out
}

// firstDifference returns a column, among those in want that undo may write,
// whose value differs between the two rows.
func firstDifference(t *table, have, want receipt.Row) string {
	for _, col := range want.Columns() {
		if t.has[col] && !receipt.Same(have[col], want[col]) {
			return col
		}
	}
	return ""
}

// droppedColumns lists columns in the receipt that the table no longer has.
func droppedColumns(t *table, changes []receipt.Change) []string {
	seen := map[string]bool{}
	for _, c := range changes {
		for _, row := range []receipt.Row{c.Old, c.New} {
			for col := range row {
				if !t.has[col] {
					seen[col] = true
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

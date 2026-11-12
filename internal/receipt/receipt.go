// Package receipt records what each agent run changed, in one JSON Lines file
// per run.
//
// A receipt is a list of transactions. Each transaction is written as one
// "change" line per changed row, followed by one "commit" line:
//
//	{"kind":"change","lsn":"0/19B9100","op":"update","relid":16385,"schema":"public","table":"customers","old":{"id":"1","plan":"pro"},"new":{"id":"1","plan":"cancelled"}}
//	{"kind":"commit","xid":770,"commit_lsn":"0/19B9510","end_lsn":"0/19B9540","time":"2026-11-10T06:37:00Z","changes":1}
//
// Lines after the last commit line belong to a transaction that was cut off
// by a crash or a lost connection. Readers ignore them and the writer removes
// them. Postgres sends that transaction again, because the recorder confirms
// a transaction only after its commit line is safely on disk.
package receipt

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// LSN is a position in Postgres's write-ahead log, printed like 0/19B9510.
type LSN uint64

func (l LSN) String() string {
	return fmt.Sprintf("%X/%X", uint32(l>>32), uint32(l))
}

// ParseLSN reads an LSN printed like 0/19B9510.
func ParseLSN(s string) (LSN, error) {
	hi, lo, ok := strings.Cut(s, "/")
	h, errHi := strconv.ParseUint(hi, 16, 32)
	l, errLo := strconv.ParseUint(lo, 16, 32)
	if !ok || errHi != nil || errLo != nil {
		return 0, fmt.Errorf("invalid LSN %q", s)
	}
	return LSN(h<<32 | l), nil
}

// MarshalText and UnmarshalText make LSNs readable in JSON.
func (l LSN) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

func (l *LSN) UnmarshalText(text []byte) error {
	v, err := ParseLSN(string(text))
	*l = v
	return err
}

// Op is the kind of change.
type Op string

const (
	Insert   Op = "insert"
	Update   Op = "update"
	Delete   Op = "delete"
	Truncate Op = "truncate" // cannot be undone: the WAL holds no old rows
)

// Row maps column names to values, printed as Postgres prints them.
// A nil value is NULL.
type Row map[string]*string

// Columns returns the row's column names, sorted.
func (r Row) Columns() []string {
	return slices.Sorted(maps.Keys(r))
}

// Same reports whether two values are identical as printed: both NULL, or
// the same text. Undo compares values this way rather than with SQL '=',
// which is looser for some types (citext, numeric scale) and missing for
// others (json, point).
func Same(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// Quote prints a value for people: NULL, or the text in quotes with control
// characters escaped. An agent may have written it, so it must not reach a
// terminal raw.
func Quote(v *string) string {
	if v == nil {
		return "NULL"
	}
	return strconv.QuoteToGraphic(*v)
}

// Change is one row changed by an agent run.
type Change struct {
	LSN    LSN    `json:"lsn"`
	Op     Op     `json:"op"`
	RelID  uint32 `json:"relid"` // the table's OID, which survives renames
	Schema string `json:"schema"`
	Table  string `json:"table"`
	Old    Row    `json:"old,omitempty"` // the row before an update or delete
	New    Row    `json:"new,omitempty"` // the row after an insert or update
}

// Commit ends a transaction in a receipt.
type Commit struct {
	XID       uint32    `json:"xid"`
	CommitLSN LSN       `json:"commit_lsn"`
	EndLSN    LSN       `json:"end_lsn"` // the position the recorder confirms
	Time      time.Time `json:"time"`
	Changes   int       `json:"changes"`
	// Incomplete explains why some of the run's changes in this transaction
	// may be missing. Undo refuses a run with an incomplete transaction.
	Incomplete string `json:"incomplete,omitempty"`
}

// Transaction is one committed transaction from a receipt.
type Transaction struct {
	Commit  Commit
	Changes []Change
}

// line is one line of a receipt file. Exactly one of Change and Commit is set;
// their JSON field names don't overlap, so both embed into the same object.
type line struct {
	Kind string `json:"kind"`
	*Change
	*Commit
}

const (
	kindChange = "change"
	kindCommit = "commit"
)

// ErrNotFound means a run has no receipt: it changed nothing that was
// recorded, or the run ID is wrong.
var ErrNotFound = errors.New("no receipt (the run changed nothing that was recorded, or the run ID is wrong)")

// runIDPattern is the rule for run IDs. The database trigger enforces the same
// rule, and the "run-" prefix keeps tool defaults such as "psql" from passing.
var runIDPattern = regexp.MustCompile(`^run-[a-z0-9][a-z0-9._-]{0,58}$`)

// ValidRunID reports whether s is a valid run ID, which also makes it safe to
// use as a file name.
func ValidRunID(s string) bool {
	return runIDPattern.MatchString(s)
}

package receipt

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLSN(t *testing.T) {
	for _, s := range []string{"0/0", "0/19B9510", "1/FFFFFFFF"} {
		l, err := ParseLSN(s)
		if err != nil || l.String() != s {
			t.Errorf("ParseLSN(%q) = %v, %v; want it to print as %q", s, l, err, s)
		}
	}
	for _, s := range []string{"", "19B9510", "0/", "0/xyz", "0/19B9510x", "100000000/0"} {
		if _, err := ParseLSN(s); err == nil {
			t.Errorf("ParseLSN(%q) succeeded, want an error", s)
		}
	}
}

func TestValidRunID(t *testing.T) {
	valid := []string{"run-42", "run-a", "run-2026-11-10.retry_2", "run-" + strings.Repeat("x", 59)}
	invalid := []string{"", "42", "run-", "Run-42", "run-42/x", "run-../x", "run-.hidden", "psql",
		"run-" + strings.Repeat("x", 60), "run-ä"}
	for _, s := range valid {
		if !ValidRunID(s) {
			t.Errorf("ValidRunID(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if ValidRunID(s) {
			t.Errorf("ValidRunID(%q) = true, want false", s)
		}
	}
}

func str(s string) *string { return &s }

func change(id string) *Change {
	return &Change{
		LSN: 0x100, Op: Update, RelID: 16385, Schema: "public", Table: "customers",
		Old: Row{"id": str(id), "plan": str("pro"), "phone": nil},
		New: Row{"id": str(id), "plan": str("cancelled"), "phone": nil},
	}
}

func commit(xid uint32, end LSN, changes int) *Commit {
	return &Commit{XID: xid, CommitLSN: end - 8, EndLSN: end, Time: time.Unix(0, 0).UTC(), Changes: changes}
}

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteAndRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	s := openStore(t, dir)
	must(t, s.Append("run-1", change("1")))
	must(t, s.Append("run-1", change("2")))
	must(t, s.Commit("run-1", commit(770, 0x200, 2)))
	must(t, s.Append("run-1", change("3"))) // never committed
	must(t, s.Sync())

	txns, err := Read(dir, "run-1")
	must(t, err)
	if len(txns) != 1 || len(txns[0].Changes) != 2 || txns[0].Commit.XID != 770 {
		t.Fatalf("Read returned %+v, want one transaction with two changes", txns)
	}
	got := txns[0].Changes[1]
	if *got.New["plan"] != "cancelled" || got.Old["phone"] != nil || got.RelID != 16385 {
		t.Errorf("change did not round-trip: %+v", got)
	}

	end, err := s.LastEnd("run-1")
	if err != nil || end != 0x200 {
		t.Errorf("LastEnd = %v, %v; want 0/200", end, err)
	}
	if _, err := Read(dir, "run-404"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read of a missing run returned %v, want ErrNotFound", err)
	}
}

// A crash can leave an unfinished transaction and a cut-off line at the end
// of a receipt. Opening the receipt again must remove both.
func TestRepairOnOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	s := openStore(t, dir)
	must(t, s.Append("run-1", change("1")))
	must(t, s.Commit("run-1", commit(1, 0x200, 1)))
	must(t, s.Sync())
	committed := fileSize(t, dir, "run-1")
	s.Close()

	f, err := os.OpenFile(path(dir, "run-1"), os.O_APPEND|os.O_WRONLY, 0)
	must(t, err)
	_, err = f.WriteString(`{"kind":"change","lsn":"0/300","op":"insert"}` + "\n" + `{"kind":"comm`)
	must(t, err)
	f.Close()

	s2 := openStore(t, dir)
	end, err := s2.LastEnd("run-1")
	if err != nil || end != 0x200 {
		t.Fatalf("LastEnd = %v, %v; want 0/200 (the cut-off lines don't count)", end, err)
	}
	// The first write removes the cut-off tail before appending; otherwise
	// the new lines would follow the half line and the receipt would be damaged.
	must(t, s2.Append("run-1", change("2")))
	must(t, s2.Commit("run-1", commit(2, 0x300, 1)))
	txns, err := Read(dir, "run-1")
	if err != nil || len(txns) != 2 {
		t.Fatalf("Read after repair = %d transactions, %v; want 2", len(txns), err)
	}
	if got := fileSize(t, dir, "run-1"); got <= committed {
		t.Errorf("file is %d bytes, want more than the %d committed before", got, committed)
	}
}

// Looking up a run must not create a receipt: statements that change no rows
// still carry a tag.
func TestLastEndCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	s := openStore(t, dir)
	if end, err := s.LastEnd("run-empty"); err != nil || end != 0 {
		t.Fatalf("LastEnd = %v, %v; want 0", end, err)
	}
	if _, err := os.Stat(path(dir, "run-empty")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LastEnd created a receipt file (stat error %v)", err)
	}
}

// More runs than maxOpen in one transaction must still work: files are closed
// and reopened without losing the open transaction's lines.
func TestManyRunsInOneTransaction(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	s := openStore(t, dir)
	n := maxOpen + 10
	for i := range n {
		must(t, s.Append(fmt.Sprintf("run-%d", i), change("1")))
	}
	for i := range n {
		must(t, s.Append(fmt.Sprintf("run-%d", i), change("2")))
		must(t, s.Commit(fmt.Sprintf("run-%d", i), commit(1, 0x200, 2)))
	}
	must(t, s.Sync())
	for _, i := range []int{0, n - 1} {
		txns, err := Read(dir, fmt.Sprintf("run-%d", i))
		if err != nil || len(txns) != 1 || len(txns[0].Changes) != 2 {
			t.Errorf("run-%d: %d transactions, %v; want one with two changes", i, len(txns), err)
		}
	}
}

func TestDiscard(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	s := openStore(t, dir)
	must(t, s.Append("run-1", change("1")))
	must(t, s.Commit("run-1", commit(1, 0x200, 1)))
	before := fileSize(t, dir, "run-1")

	big := change("2")
	big.New["notes"] = str(strings.Repeat("y", 1<<20)) // larger than the write buffer
	must(t, s.Append("run-1", big))
	must(t, s.Discard())
	if got := fileSize(t, dir, "run-1"); got != before {
		t.Errorf("file is %d bytes after Discard, want %d", got, before)
	}
}

// A damaged line in the middle of committed data is not a cut-off write, so
// it must be an error instead of being skipped.
func TestCorruptCommittedLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	must(t, os.MkdirAll(dir, 0o700))
	content := `{"kind":"change","lsn":"0/100","op":"insert","relid":1,"schema":"public","table":"t","new":{"id":"1"}}
not json
{"kind":"commit","xid":1,"commit_lsn":"0/1F8","end_lsn":"0/200","time":"2026-01-01T00:00:00Z","changes":2}
`
	must(t, os.WriteFile(path(dir, "run-1"), []byte(content), 0o600))
	if _, err := Read(dir, "run-1"); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("Read = %v, want an error about line 2", err)
	}
}

func TestLargeValue(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	s := openStore(t, dir)
	c := change("1")
	c.New["body"] = str(strings.Repeat("x", 200_000) + "<&>")
	must(t, s.Append("run-1", c))
	must(t, s.Commit("run-1", commit(1, 0x200, 1)))
	txns, err := Read(dir, "run-1")
	must(t, err)
	if got := *txns[0].Changes[0].New["body"]; got != *c.New["body"] {
		t.Errorf("a 200 KB value did not round-trip (got %d bytes)", len(got))
	}
}

func TestOneStorePerDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory locking is Unix-only")
	}
	dir := filepath.Join(t.TempDir(), "receipts")
	openStore(t, dir)
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("second Open = %v, want an in-use error", err)
	}
}

func TestRefusesSharedDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	must(t, os.Mkdir(dir, 0o755))
	must(t, os.Chmod(dir, 0o755)) // the umask may have narrowed Mkdir
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Errorf("Open of a 0755 directory = %v, want a permissions error", err)
	}
}

func TestState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	if _, err := ReadState(dir); !errors.Is(err, ErrNoState) {
		t.Fatalf("ReadState before init = %v, want ErrNoState", err)
	}
	want := State{SystemID: "7706603552223293478", Timeline: 1, Database: "shop", Slot: "undo_slot",
		Publication: "undo_pub", AgentRole: "agent_bot", Confirmed: 0x19B9540,
		Gaps: []Gap{{From: 1, To: 2, Time: time.Unix(0, 0).UTC()}}}
	store := openStore(t, dir)
	must(t, store.SaveState(want))
	got, err := ReadState(dir)
	must(t, err)
	if got.SystemID != want.SystemID || got.Confirmed != want.Confirmed || len(got.Gaps) != 1 || got.Gaps[0].To != 2 {
		t.Errorf("ReadState = %+v, want %+v", got, want)
	}
}

func TestRuns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "receipts")
	s := openStore(t, dir)
	for _, run := range []string{"run-b", "run-a"} {
		must(t, s.Append(run, change("1")))
		must(t, s.Commit(run, commit(1, 0x200, 1)))
	}
	runs, err := Runs(dir)
	if err != nil || strings.Join(runs, ",") != "run-a,run-b" {
		t.Errorf("Runs = %v, %v; want [run-a run-b]", runs, err)
	}
}

func fileSize(t *testing.T, dir, run string) int64 {
	t.Helper()
	info, err := os.Stat(path(dir, run))
	must(t, err)
	return info.Size()
}

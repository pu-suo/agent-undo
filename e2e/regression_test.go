package e2e

// Tests for problems found in review. Each one failed before its fix.

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pu-suo/agent-undo/internal/capture"
	"github.com/pu-suo/agent-undo/internal/setup"
	"github.com/pu-suo/agent-undo/internal/undo"
)

const parentChildSchema = `
create table p (id int primary key, name text);
create table c (id int primary key, p_id int references p(id) on delete cascade);
insert into p values (1, 'one'); insert into c values (1, 1);
grant select, insert, update, delete on p, c to undo_test_agent;
`

// runAndCompare runs the agent and people's SQL, undoes the run, and returns
// the plan and the differences from a control where the agent never ran.
func runAndCompare(t *testing.T, schema string, agentSQL, peopleSQL []string) (*undo.Plan, []string) {
	t.Helper()
	control := newControl(t, schema)
	d := newDB(t, schema)
	d.startRecorder(t)
	if err := d.agent(t, "run-1", agentSQL...); err != nil {
		t.Fatal(err)
	}
	for _, sql := range peopleSQL {
		d.exec(t, sql)
		control.tryExec(t, sql)
	}
	d.waitCaughtUp(t)
	previewed := d.preview(t, "run-1")
	applied, err := d.apply(t, "run-1", previewed.Hash)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return applied, diff(snapshot(t, d), snapshot(t, control))
}

func TestUndoRemovesRowsTheAgentInserted(t *testing.T) {
	plan, differences := runAndCompare(t, parentChildSchema,
		[]string{`insert into p values (10, 'new')`, `insert into c values (10, 10)`}, nil)
	if len(differences) > 0 || plan.NeedsReview() {
		t.Errorf("differs from the control: %v; plan %+v", differences, plan.Rows)
	}
}

// A person adds a child row under the agent's new parent. Deleting the parent
// would cascade to the person's row, so undo keeps it, and preview must say
// so too: the same hash has to mean the same outcome.
func TestPersonsChildKeepsTheAgentsParent(t *testing.T) {
	d := newDB(t, parentChildSchema)
	d.startRecorder(t)
	if err := d.agent(t, "run-1", `insert into p values (11, 'new')`); err != nil {
		t.Fatal(err)
	}
	d.exec(t, `insert into c values (11, 11)`)
	d.waitCaughtUp(t)
	previewed := d.preview(t, "run-1")
	if len(previewed.Rows) != 1 || previewed.Rows[0].Action != undo.Keep {
		t.Fatalf("preview = %+v, want the parent kept", previewed.Rows)
	}
	applied, err := d.apply(t, "run-1", previewed.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Rows[0].Action != undo.Keep || len(d.query(t, `select 1 from c where id = 11`)) != 1 {
		t.Errorf("apply = %+v; the person's child row must survive", applied.Rows)
	}
}

func TestIdentityColumnRowIsPutBack(t *testing.T) {
	schema := `create table t (id int generated always as identity primary key, v text);
		insert into t (v) values ('a'), ('b');
		grant select, insert, update, delete on t to undo_test_agent;`
	plan, differences := runAndCompare(t, schema, []string{`delete from t where id = 1`}, nil)
	if len(differences) > 0 || plan.NeedsReview() {
		t.Errorf("differs from the control: %v; plan %+v", differences, plan.Rows)
	}
}

// Restoring a value that other tables refer to with ON UPDATE CASCADE would
// change their rows, so undo must not do it silently.
func TestOnUpdateCascadeIsNotSilent(t *testing.T) {
	schema := `create table p (id int primary key, code text unique);
		create table c (id int primary key, p_code text references p(code) on update cascade);
		insert into p values (1, 'a'); insert into c values (1, 'a');
		grant select, insert, update, delete on p, c to undo_test_agent;`
	plan, differences := runAndCompare(t, schema, []string{`update p set code = 'b' where id = 1`}, nil)
	if len(differences) > 0 && !plan.NeedsReview() {
		t.Errorf("silently wrong: %v", differences)
	}
	if !plan.NeedsReview() {
		t.Errorf("plan %+v: want the cascading key refused", plan.Rows)
	}
}

// The agent deleted a row and inserted a new one with the same key; then a
// person deleted it. Undo must not bring the original back.
func TestRecreatedRowDeletedByAPersonStaysDeleted(t *testing.T) {
	plan, differences := runAndCompare(t, parentChildSchema,
		[]string{`begin`, `delete from c where id = 1`, `delete from p where id = 1`, `insert into p values (1, 'remade')`, `commit`},
		[]string{`delete from p where id = 1`})
	if len(differences) > 0 && !plan.NeedsReview() {
		t.Errorf("silently wrong: %v", differences)
	}
	if got := len(plan.Rows); got == 0 {
		t.Fatal("empty plan")
	}
}

func TestRenamedColumnIsRefused(t *testing.T) {
	plan, differences := runAndCompare(t, parentChildSchema,
		[]string{`update p set name = 'agent' where id = 1`},
		[]string{`alter table p rename column name to title`})
	if !plan.NeedsReview() || len(differences) == 0 {
		t.Errorf("plan %+v, differences %v: want the renamed column refused, not silently skipped", plan.Rows, differences)
	}
}

// A REPEATABLE READ transaction that began before the seal still sees the run
// as unsealed. Its writes must fail anyway.
func TestSealStopsTransactionsThatBeganBefore(t *testing.T) {
	d := newDB(t, parentChildSchema)
	d.startRecorder(t)
	if err := d.agent(t, "run-1", `update p set name = 'agent' where id = 1`); err != nil {
		t.Fatal(err)
	}
	early := d.agentConn(t, "run-1")
	ctx := context.Background()
	if _, err := early.Exec(ctx, `begin isolation level repeatable read`); err != nil {
		t.Fatal(err)
	}
	if _, err := early.Exec(ctx, `select count(*) from p`); err != nil { // takes the snapshot
		t.Fatal(err)
	}

	d.waitCaughtUp(t)
	if _, err := d.apply(t, "run-1", d.preview(t, "run-1").Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := early.Exec(ctx, `update p set name = 'late' where id = 1`); err == nil {
		t.Error("a transaction from before the seal wrote after the undo")
	}
	_, _ = early.Exec(ctx, `rollback`) // the transaction already failed
}

// A run with an open transaction that already wrote can't be sealed until
// that transaction ends.
func TestSealWaitsForOpenTransactions(t *testing.T) {
	d := newDB(t, parentChildSchema)
	d.startRecorder(t)
	open := d.agentConn(t, "run-1")
	ctx := context.Background()
	for _, sql := range []string{`begin`, `update p set name = 'agent' where id = 1`} {
		if _, err := open.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	_, err := undo.Apply(ctx, d.undoConn(t), undo.Options{Receipts: d.receipts, Run: "run-1", Wait: time.Second})
	if err == nil || !strings.Contains(err.Error(), "open transaction") {
		t.Errorf("apply during an open agent transaction = %v, want it to wait and fail", err)
	}
	if _, err := open.Exec(ctx, `commit`); err != nil {
		t.Fatal(err)
	}
}

func TestTeardownWhileRecordingRemovesNothing(t *testing.T) {
	d := newDB(t, parentChildSchema)
	d.startRecorder(t)
	d.waitStreaming(t)
	conn := d.undoConn(t)
	if _, err := setup.Teardown(context.Background(), conn); err == nil || !strings.Contains(err.Error(), "stop undo record first") {
		t.Errorf("teardown while recording = %v, want it refused", err)
	}
	if _, err := setup.ReadSettings(context.Background(), conn); err != nil {
		t.Errorf("teardown removed agent-undo anyway: %v", err)
	}
}

func TestInitRefusesAnotherDatabasesSlot(t *testing.T) {
	first := newDB(t, parentChildSchema)
	second := createDB(t, parentChildSchema)
	settings := first.settings // the same slot name
	_, err := setup.Init(context.Background(), second.cfg, t.TempDir()+"/receipts", settings, false)
	if err == nil || !strings.Contains(err.Error(), "belongs to database") {
		t.Errorf("init with another database's slot = %v, want it refused", err)
	}
}

// Cancelling the walsender (a common "kill long queries" job would) must not
// stop recording for good.
func TestRecorderSurvivesBeingCancelled(t *testing.T) {
	d := newDB(t, parentChildSchema)
	ctx, cancel := context.WithCancel(context.Background())
	rec := &capture.Recorder{Config: d.cfg, Receipts: d.receipts, Log: slog.New(slog.NewTextHandler(t.Output(), nil))}
	done := make(chan error, 1)
	go func() { done <- rec.Record(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("recorder: %v", err)
		}
	})
	d.waitCaughtUp(t)

	d.exec(t, `select pg_cancel_backend(active_pid) from pg_replication_slots where slot_name = '`+d.settings.Slot+`'`)
	if err := d.agent(t, "run-1", `update p set name = 'after cancel' where id = 1`); err != nil {
		t.Fatal(err)
	}
	d.waitCaughtUp(t)
	if txns := d.read(t, "run-1"); len(txns) != 1 {
		t.Errorf("got %d transactions after the cancel, want 1", len(txns))
	}
}

// waitStreaming waits until a recorder is reading the slot.
func (d *testDB) waitStreaming(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got := d.query(t, `select active from pg_replication_slots where slot_name = '`+d.settings.Slot+`'`); len(got) == 1 && got[0] == "t" {
			return
		}
	}
	t.Fatal("the recorder did not start streaming within 20 seconds")
}

// agentConn opens a connection as the agent and keeps it open for the test.
func (d *testDB) agentConn(t *testing.T, run string) *pgx.Conn {
	t.Helper()
	cfg := d.userConfig()
	cfg.User, cfg.Password = agentRole, agentRole
	cfg.RuntimeParams["application_name"] = run
	conn := d.connect(t, cfg)
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// A setup problem, such as a table created after init, is reported but must
// not stop recording: refusing to record would only lose more changes.
func TestRecorderStartsDespiteSetupProblems(t *testing.T) {
	d := newDB(t, parentChildSchema)
	d.exec(t, `create table notes (id int primary key)`)
	d.startRecorder(t)
	d.waitStreaming(t)
	if err := d.agent(t, "run-1", `update p set name = 'x' where id = 1`); err != nil {
		t.Fatal(err)
	}
	d.waitCaughtUp(t)
	if len(d.read(t, "run-1")) != 1 {
		t.Error("the recorder did not record while a table was untagged")
	}
}

// When something moves the slot past the receipts, recording stops; init
// --new-epoch starts again and marks the gap, and undo refuses a run that
// started before the gap, because some of its changes may be missing.
func TestMovedSlotStartsANewEpoch(t *testing.T) {
	d := newDB(t, parentChildSchema)
	stop := d.startRecorder(t)
	if err := d.agent(t, "run-early", `update p set name = 'a' where id = 1`); err != nil {
		t.Fatal(err)
	}
	d.waitCaughtUp(t)
	stop()

	// The agent writes while the recorder is down, then something advances
	// the slot past that write, so it is never recorded.
	if err := d.agent(t, "run-early", `update p set name = 'b' where id = 1`); err != nil {
		t.Fatal(err)
	}
	d.exec(t, `select pg_replication_slot_advance('`+d.settings.Slot+`', pg_current_wal_lsn())`)

	ctx := context.Background()
	rec := &capture.Recorder{Config: d.cfg, Receipts: d.receipts, Log: slog.New(slog.NewTextHandler(t.Output(), nil))}
	if err := rec.Run(ctx); err == nil || !strings.Contains(err.Error(), "was moved") {
		t.Fatalf("recorder after the slot moved = %v, want it to refuse", err)
	}
	if _, err := setup.Init(ctx, d.cfg, d.receipts, d.settings, false); err == nil {
		t.Fatal("init without --new-epoch accepted a moved slot")
	}
	res, err := setup.Init(ctx, d.cfg, d.receipts, d.settings, true)
	if err != nil || res.Gap == nil {
		t.Fatalf("init --new-epoch = %+v, %v; want a recorded gap", res, err)
	}

	d.startRecorder(t)
	d.waitCaughtUp(t)
	if plan := d.preview(t, "run-early"); plan.Refused == "" {
		t.Errorf("a run that started before the gap was not refused: %+v", plan.Rows)
	}
}

// Package e2e tests agent-undo against a real Postgres server with
// wal_level=logical, such as the one in docker-compose.yml. Each test creates
// its own database and replication slot and removes them afterwards.
//
// The server comes from AGENT_UNDO_TEST_DB, an admin connection string
// (default: the docker compose server). go test -short skips these tests.
package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pu-suo/agent-undo/internal/capture"
	"github.com/pu-suo/agent-undo/internal/db"
	"github.com/pu-suo/agent-undo/internal/receipt"
	"github.com/pu-suo/agent-undo/internal/setup"
)

const (
	defaultAdminURL = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	agentRole       = "undo_test_agent" // a login role only these tests use
)

// adminURL is the connection string of an admin on the test server.
func adminURL() string {
	if url := os.Getenv("AGENT_UNDO_TEST_DB"); url != "" {
		return url
	}
	return defaultAdminURL
}

// testDB is a fresh database set up for recording.
type testDB struct {
	name     string
	cfg      *pgx.ConnConfig // an admin connection to this database
	receipts string
	settings setup.Settings
}

// newDB creates a database, runs schema in it as an admin, and sets up
// recording with a receipts directory of its own.
func newDB(t *testing.T, schema string) *testDB {
	t.Helper()
	d := createDB(t, schema)
	d.receipts = filepath.Join(t.TempDir(), "receipts")
	d.settings = setup.Settings{AgentRole: agentRole, Slot: d.name, Publication: "undo_pub"}
	if _, err := setup.Init(context.Background(), d.cfg, d.receipts, d.settings, false); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, f := range check(t, d) {
		if strings.HasPrefix(f, "ERROR") {
			t.Fatalf("undo check after init: %s", f)
		}
	}
	return d
}

// newTwin creates a second database with the same schema and no recording,
// where the agent never runs. After undo, the recorded database should
// match it.
func newTwin(t *testing.T, schema string) *testDB {
	t.Helper()
	return createDB(t, schema)
}

// createDB creates a database and runs schema in it as an admin.
func createDB(t *testing.T, schema string) *testDB {
	t.Helper()
	if testing.Short() {
		t.Skip("needs Postgres; skipped with -short")
	}
	ctx := context.Background()
	adminCfg, err := db.Config(adminURL())
	if err != nil {
		t.Fatal(err)
	}
	admin, err := db.Connect(ctx, adminCfg)
	if err != nil {
		t.Fatalf("%v\n(start the database with: docker compose up -d)", err)
	}
	defer admin.Close(ctx)

	ensureAgentRole(t, admin)
	d := &testDB{name: "undo_test_" + randomSuffix()}
	mustExec(t, admin, `create database `+pgx.Identifier{d.name}.Sanitize())
	t.Cleanup(func() { d.drop(t, adminCfg) })
	d.cfg = adminCfg.Copy()
	d.cfg.Database = d.name
	// Postgres 14 lets everyone create tables in public; the agent mustn't.
	d.exec(t, `revoke create on schema public from public`)
	d.exec(t, schema)
	return d
}

func ensureAgentRole(t *testing.T, admin *pgx.Conn) {
	t.Helper()
	_, err := admin.Exec(context.Background(), `
		do $$ begin
			create role undo_test_agent login password 'undo_test_agent';
		exception when duplicate_object then null;
		end $$`)
	if err != nil {
		t.Fatal(err)
	}
}

// drop removes the test's slot and database. The slot can stay active for a
// moment after the recorder disconnects, so dropping it is retried.
func (d *testDB) drop(t *testing.T, adminCfg *pgx.ConnConfig) {
	ctx := context.Background()
	admin, err := db.Connect(ctx, adminCfg)
	if err != nil {
		t.Error(err)
		return
	}
	defer admin.Close(ctx)
	for deadline := time.Now().Add(15 * time.Second); d.settings.Slot != ""; {
		err := setup.DropSlot(ctx, admin, d.settings.Slot)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("drop slot %s: %v", d.settings.Slot, err)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	mustExec(t, admin, `drop database `+pgx.Identifier{d.name}.Sanitize()+` with (force)`)
}

// userConfig is how a person or an agent connects: like agent-undo, but with
// the usual search_path, so table names need no schema.
func (d *testDB) userConfig() *pgx.ConnConfig {
	cfg := d.cfg.Copy()
	cfg.RuntimeParams["search_path"] = "public"
	cfg.RuntimeParams["application_name"] = "psql"
	return cfg
}

// exec runs SQL in the test database as a person (the admin), outside any
// agent run.
func (d *testDB) exec(t *testing.T, sql string) {
	t.Helper()
	conn := d.connect(t, d.userConfig())
	defer conn.Close(context.Background())
	if _, err := conn.PgConn().Exec(context.Background(), sql).ReadAll(); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// agent runs each statement in its own transaction as the agent role, with
// application_name set to run, and returns the first error.
func (d *testDB) agent(t *testing.T, run string, statements ...string) error {
	t.Helper()
	cfg := d.userConfig()
	cfg.User, cfg.Password = agentRole, agentRole
	cfg.RuntimeParams["application_name"] = run
	conn := d.connect(t, cfg)
	defer conn.Close(context.Background())
	for _, sql := range statements {
		if _, err := conn.Exec(context.Background(), sql); err != nil {
			return err
		}
	}
	return nil
}

func (d *testDB) connect(t *testing.T, cfg *pgx.ConnConfig) *pgx.Conn {
	t.Helper()
	conn, err := db.Connect(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// query returns the first column of every row, as text.
func (d *testDB) query(t *testing.T, sql string) []string {
	t.Helper()
	conn := d.connect(t, d.userConfig())
	defer conn.Close(context.Background())
	rows, err := conn.Query(context.Background(), sql, pgx.QueryResultFormats{pgx.TextFormatCode})
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var s *string
		err := r.Scan(&s)
		if s == nil {
			return "NULL", err
		}
		return *s, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// startRecorder runs a recorder until the returned function (or the end of
// the test) stops it.
func (d *testDB) startRecorder(t *testing.T) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	rec := &capture.Recorder{Config: d.cfg, Receipts: d.receipts, Log: slog.New(slog.NewTextHandler(t.Output(), nil))}
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("recorder: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// waitCaughtUp waits until the recorder has confirmed everything committed so
// far, which means it is all in the receipts.
func (d *testDB) waitCaughtUp(t *testing.T) {
	t.Helper()
	target := d.query(t, `select pg_current_wal_lsn()`)[0]
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		ok := d.query(t, `select confirmed_flush_lsn >= '`+target+`' from pg_replication_slots where slot_name = '`+d.settings.Slot+`'`)
		if len(ok) == 1 && ok[0] == "t" {
			return
		}
	}
	t.Fatal("the recorder did not catch up within 20 seconds")
}

func (d *testDB) read(t *testing.T, run string) []receipt.Transaction {
	t.Helper()
	txns, err := receipt.Read(d.receipts, run)
	if err != nil && !errors.Is(err, receipt.ErrNotFound) {
		t.Fatal(err)
	}
	return txns
}

// check runs the setup checks and returns the findings as text.
func check(t *testing.T, d *testDB) []string {
	t.Helper()
	conn := d.connect(t, d.cfg)
	defer conn.Close(context.Background())
	_, findings, err := setup.Check(context.Background(), conn)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range findings {
		out = append(out, f.String())
	}
	return out
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func randomSuffix() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

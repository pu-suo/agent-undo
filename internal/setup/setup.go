// Package setup installs agent-undo in a database and checks that recording
// covers everything the agent can write.
//
// Install tags every permanent table outside the system schemas: it gives
// each one REPLICA IDENTITY FULL, so the WAL keeps whole old rows, adds the
// tag trigger and publishes it. Tagging every table, rather than only those
// the agent may write today, means no path can escape: column grants,
// writable views, functions that run as their owner, and triggers on other
// tables all end up writing to a tagged table. Check verifies the setup later,
// because tables and grants change over time.
//
// Connections use an empty search_path, so queries name agent-undo's objects
// with their schema. System catalogs are always searched first.
package setup

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pu-suo/agent-undo/internal/db"
	"github.com/pu-suo/agent-undo/internal/receipt"
)

//go:embed install.sql
var installSQL string

// Settings names the agent's role, the replication slot and the publication.
// They are stored in agent_undo.settings.
type Settings struct {
	AgentRole   string
	Slot        string
	Publication string
}

// namePattern restricts slot and publication names, which end up unquoted in
// replication commands.
var namePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func (s Settings) validate() error {
	if !namePattern.MatchString(s.Slot) || !namePattern.MatchString(s.Publication) {
		return fmt.Errorf("slot and publication names must match %s", namePattern)
	}
	if s.AgentRole == "" {
		return errors.New("the agent role name is empty")
	}
	return nil
}

// tablesQuery lists the tables to tag: every permanent table and partitioned
// table outside the system schemas. Unlogged and temporary tables are not
// written to the WAL, so they can't be recorded; Check reports them if the
// agent can write them. In the trigger test, tgtype 62 means BEFORE, FOR EACH
// STATEMENT, on INSERT, UPDATE, DELETE and TRUNCATE, and tgenabled 'A' means
// ENABLE ALWAYS.
const tablesQuery = `
select n.nspname, c.relname, c.relkind::text, c.relispartition, c.relreplident = 'f',
	exists (select from pg_trigger g
	        where g.tgrelid = c.oid and g.tgname = 'agent_undo_tag'
	          and g.tgfoid = to_regproc('agent_undo.tag_statement')
	          and g.tgenabled = 'A' and g.tgtype = 62)
from pg_class c
join pg_namespace n on n.oid = c.relnamespace
where c.relkind in ('r', 'p') and c.relpersistence = 'p'
  and n.nspname not in ('information_schema', 'agent_undo') and n.nspname not like 'pg\_%'
order by 1, 2`

type table struct {
	schema, name string
	kind         string // "r" for a table, "p" for a partitioned table
	partition    bool   // a partition of a partitioned table
	fullIdentity bool   // REPLICA IDENTITY FULL
	tagged       bool   // has agent-undo's trigger, enabled always
}

func (t table) ident() string { return pgx.Identifier{t.schema, t.name}.Sanitize() }

// Install creates agent-undo's objects and tags every table, all in one
// transaction, and returns the tagged tables. Run it again after creating
// tables: tables that are already set up are left alone.
func Install(ctx context.Context, conn *pgx.Conn, s Settings) ([]string, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	var super bool
	err := conn.QueryRow(ctx, `select rolsuper from pg_roles where rolname = $1`, s.AgentRole).Scan(&super)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("role %q does not exist", s.AgentRole)
	}
	if err != nil {
		return nil, err
	}
	if super {
		return nil, fmt.Errorf("role %q is a superuser, which could switch the trigger off", s.AgentRole)
	}

	var names []string
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		// Altering a table waits for its running queries. Give up instead of
		// queueing everyone else behind init.
		if _, err := tx.Exec(ctx, `set local lock_timeout = '5s'`); err != nil {
			return err
		}
		// install.sql holds several statements, so it goes through the simple
		// protocol. It has no parameters, so nothing is interpolated.
		if _, err := tx.Conn().PgConn().Exec(ctx, installSQL).ReadAll(); err != nil {
			return fmt.Errorf("install agent-undo objects: %w", err)
		}
		_, err := tx.Exec(ctx, `
			insert into agent_undo.settings (agent_role, slot_name, publication) values ($1, $2, $3)
			on conflict (id) do update set agent_role = excluded.agent_role,
				slot_name = excluded.slot_name, publication = excluded.publication`,
			s.AgentRole, s.Slot, s.Publication)
		if err != nil {
			return err
		}

		tables, err := listTables(ctx, tx)
		if err != nil {
			return err
		}
		published := []string{"only " + pgx.Identifier{"agent_undo", "tag"}.Sanitize()}
		for _, t := range tables {
			if err := track(ctx, tx, t); err != nil {
				return err
			}
			// A partitioned table publishes its partitions, including ones
			// created later; each change arrives under its partition's name.
			if !t.partition {
				published = append(published, "only "+t.ident())
			}
			names = append(names, t.schema+"."+t.name)
		}
		return publish(ctx, tx, s.Publication, published)
	})
	if err != nil && strings.Contains(err.Error(), "lock timeout") {
		err = fmt.Errorf("%w: a long-running query is holding a table; try again when it finishes", err)
	}
	return names, err
}

func listTables(ctx context.Context, tx pgx.Tx) ([]table, error) {
	rows, err := tx.Query(ctx, tablesQuery)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (table, error) {
		var t table
		err := r.Scan(&t.schema, &t.name, &t.kind, &t.partition, &t.fullIdentity, &t.tagged)
		return t, err
	})
}

// track makes one table recordable: whole old rows in the WAL, and the tag
// trigger, enabled even for sessions that switch ordinary triggers off. It
// skips what is already in place, because each ALTER locks the table.
func track(ctx context.Context, tx pgx.Tx, t table) error {
	var statements []string
	if !t.fullIdentity {
		statements = append(statements, `alter table `+t.ident()+` replica identity full`)
	}
	if !t.tagged {
		statements = append(statements,
			`create or replace trigger agent_undo_tag
				before insert or update or delete or truncate on `+t.ident()+`
				for each statement execute function agent_undo.tag_statement()`,
			`alter table `+t.ident()+` enable always trigger agent_undo_tag`)
	}
	for _, sql := range statements {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("track %s.%s: %w", t.schema, t.name, err)
		}
	}
	return nil
}

func publish(ctx context.Context, tx pgx.Tx, publication string, tables []string) error {
	var exists bool
	err := tx.QueryRow(ctx, `select exists (select from pg_publication where pubname = $1)`, publication).Scan(&exists)
	if err != nil {
		return err
	}
	pub := pgx.Identifier{publication}.Sanitize()
	options := `(publish = 'insert, update, delete, truncate', publish_via_partition_root = false)`
	statements := []string{
		`alter publication ` + pub + ` set ` + options,
		`alter publication ` + pub + ` set table ` + strings.Join(tables, ", "),
	}
	if !exists {
		statements[0] = `create publication ` + pub + ` with ` + options
	}
	for _, sql := range statements {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return fmt.Errorf("publish tables: %w", err)
		}
	}
	return nil
}

// Slot describes a replication slot.
type Slot struct {
	Exists    bool
	Plugin    string
	Database  string
	Active    bool
	WALStatus string
	Confirmed receipt.LSN // confirmed_flush_lsn: where reading resumes
}

// queryRower is satisfied by both *pgx.Conn and pgx.Tx.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ReadSlot looks up a replication slot by name.
func ReadSlot(ctx context.Context, q queryRower, name string) (Slot, error) {
	s := Slot{Exists: true}
	var confirmed *string
	err := q.QueryRow(ctx, `
		select plugin, coalesce(database, ''), active, coalesce(wal_status, ''), confirmed_flush_lsn::text
		from pg_replication_slots where slot_name = $1`, name).
		Scan(&s.Plugin, &s.Database, &s.Active, &s.WALStatus, &confirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Slot{}, nil
	}
	if err != nil {
		return Slot{}, fmt.Errorf("read replication slot %s: %w", name, err)
	}
	if confirmed != nil {
		if s.Confirmed, err = receipt.ParseLSN(*confirmed); err != nil {
			return Slot{}, err
		}
	}
	return s, nil
}

// Server identifies a Postgres server and the history it is on.
type Server struct {
	SystemID string
	Timeline int32
	Database string
	WALEnd   receipt.LSN
}

// Identify asks the server for its identity and current WAL end over a
// replication connection.
func Identify(ctx context.Context, conn *pgconn.PgConn) (Server, error) {
	sys, err := pglogrepl.IdentifySystem(ctx, conn)
	if err != nil {
		return Server{}, fmt.Errorf("identify system: %w", err)
	}
	return Server{SystemID: sys.SystemID, Timeline: sys.Timeline, Database: sys.DBName, WALEnd: receipt.LSN(sys.XLogPos)}, nil
}

// connectAndIdentify opens a short replication connection to identify the
// server.
func connectAndIdentify(ctx context.Context, cfg *pgx.ConnConfig) (Server, error) {
	conn, err := db.ConnectReplication(ctx, cfg)
	if err != nil {
		return Server{}, err
	}
	defer conn.Close(context.Background())
	return Identify(ctx, conn)
}

// CreateSlot creates a logical replication slot for pgoutput and returns the
// server's identity and the position recording starts from. Creating a slot
// waits for transactions that are already running, so ctx should carry a
// timeout.
func CreateSlot(ctx context.Context, cfg *pgx.ConnConfig, name string) (Server, receipt.LSN, error) {
	if !namePattern.MatchString(name) {
		return Server{}, 0, fmt.Errorf("invalid slot name %q", name)
	}
	conn, err := db.ConnectReplication(ctx, cfg)
	if err != nil {
		return Server{}, 0, err
	}
	defer conn.Close(context.Background())

	server, err := Identify(ctx, conn)
	if err != nil {
		return Server{}, 0, err
	}
	result, err := pglogrepl.CreateReplicationSlot(ctx, conn, name, "pgoutput",
		pglogrepl.CreateReplicationSlotOptions{SnapshotAction: "NOEXPORT_SNAPSHOT"})
	if err != nil {
		return Server{}, 0, fmt.Errorf("create replication slot %s: %w", name, err)
	}
	start, err := receipt.ParseLSN(result.ConsistentPoint)
	return server, start, err
}

// DropSlot removes a replication slot so it stops holding WAL. It fails if a
// recorder is reading the slot.
func DropSlot(ctx context.Context, conn *pgx.Conn, name string) error {
	_, err := conn.Exec(ctx, `select pg_drop_replication_slot($1)`, name)
	switch {
	case hasCode(err, "42704"): // undefined_object: already gone
		return nil
	case hasCode(err, "55006"): // object_in_use
		return fmt.Errorf("replication slot %s is in use: stop undo record first", name)
	}
	return err
}

// ReadSettings returns the settings undo init stored, or ErrNotInstalled.
func ReadSettings(ctx context.Context, q queryRower) (Settings, error) {
	var s Settings
	err := q.QueryRow(ctx, `select agent_role, slot_name, publication from agent_undo.settings`).
		Scan(&s.AgentRole, &s.Slot, &s.Publication)
	if hasCode(err, "42P01") || errors.Is(err, pgx.ErrNoRows) { // undefined_table, or no row
		return Settings{}, ErrNotInstalled
	}
	return s, err
}

// ErrNotInstalled means undo init has not run in this database.
var ErrNotInstalled = errors.New("agent-undo is not installed in this database: run undo init")

// Teardown removes the replication slot, then the triggers, the publication
// and the agent_undo schema. It leaves receipts and REPLICA IDENTITY alone.
// The slot goes first: it fails while a recorder is reading it, and then
// nothing has been removed yet.
func Teardown(ctx context.Context, conn *pgx.Conn) (Settings, error) {
	s, err := ReadSettings(ctx, conn)
	if err != nil {
		return Settings{}, err
	}
	if err := DropSlot(ctx, conn, s.Slot); err != nil {
		return Settings{}, err
	}
	return s, pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			select n.nspname, c.relname
			from pg_trigger t
			join pg_class c on c.oid = t.tgrelid
			join pg_namespace n on n.oid = c.relnamespace
			where t.tgfoid = 'agent_undo.tag_statement'::regproc`)
		if err != nil {
			return err
		}
		tables, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (table, error) {
			var t table
			err := r.Scan(&t.schema, &t.name)
			return t, err
		})
		if err != nil {
			return err
		}
		var statements []string
		for _, t := range tables {
			statements = append(statements, `drop trigger agent_undo_tag on `+t.ident())
		}
		statements = append(statements,
			`drop publication if exists `+pgx.Identifier{s.Publication}.Sanitize(),
			`drop schema agent_undo cascade`)
		for _, sql := range statements {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return err
			}
		}
		return nil
	})
}

// hasCode reports whether err is a Postgres error with the given SQLSTATE.
func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

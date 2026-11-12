// Package capture reads Postgres's logical replication stream and writes each
// agent run's changes to its receipt.
//
// The one rule that keeps receipts complete: the recorder tells Postgres it
// has a position (a "standby status update") only after every change up to
// that position is synced to disk. Postgres keeps everything after the last
// confirmed position and sends it again after a restart, and the recorder
// skips what its receipts already hold.
package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/pu-suo/agent-undo/internal/db"
	"github.com/pu-suo/agent-undo/internal/receipt"
	"github.com/pu-suo/agent-undo/internal/setup"
)

const (
	// heartbeat is how often the recorder reports to Postgres when nothing
	// has changed. Postgres drops a replication connection that is silent
	// for wal_sender_timeout (60 seconds by default).
	heartbeat = 10 * time.Second
	// confirmDelay is how long new progress waits before it is confirmed,
	// so a busy stream syncs about once a second instead of per transaction.
	confirmDelay = time.Second
)

// Recorder writes receipts for one database.
type Recorder struct {
	Config   *pgx.ConnConfig
	Receipts string // the receipts directory, set up by undo init
	Log      *slog.Logger
}

// errSlotActive means another connection is reading the slot, possibly a
// previous recorder whose connection hasn't timed out yet.
var errSlotActive = errors.New("the replication slot is in use by another connection")

// Record runs the recorder until ctx is cancelled. After errors that a later
// attempt can fix, such as a lost connection, it waits and starts again;
// other errors stop it.
func (r *Recorder) Record(ctx context.Context) error {
	wait := time.Second
	for {
		started := time.Now()
		err := r.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
		if time.Since(started) > time.Minute {
			wait = time.Second // it was healthy for a while
		}
		r.Log.Warn("recording stopped; reconnecting", "error", err, "in", wait)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Minute)
	}
}

// Run records once: it checks the setup, streams until ctx is cancelled or an
// error occurs, and returns. Each call starts from what is on disk. It
// returns nil when ctx is cancelled.
func (r *Recorder) Run(ctx context.Context) error {
	err := r.runOnce(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (r *Recorder) runOnce(ctx context.Context) error {
	st, err := receipt.ReadState(r.Receipts)
	if err != nil {
		return err
	}
	store, err := receipt.Open(r.Receipts)
	if err != nil {
		return err
	}
	defer store.Close() // also drops a transaction that was cut off

	if err := r.preflight(ctx, st); err != nil {
		return err
	}

	conn, err := db.ConnectReplication(ctx, r.Config)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	server, err := setup.Identify(ctx, conn)
	if err != nil {
		return err
	}
	switch {
	case server.SystemID != st.SystemID || server.Timeline != st.Timeline:
		return fmt.Errorf("the server is not the one these receipts were recorded from (system %s timeline %d, expected %s timeline %d)",
			server.SystemID, server.Timeline, st.SystemID, st.Timeline)
	case server.WALEnd < st.Confirmed:
		// For example, the database was restored from a snapshot. Starting
		// here would silently skip the commits made since.
		return fmt.Errorf("the server's WAL ends at %s, before the receipts' %s; was the database restored?", server.WALEnd, st.Confirmed)
	}

	err = pglogrepl.StartReplication(ctx, conn, st.Slot, pglogrepl.LSN(st.Confirmed), pglogrepl.StartReplicationOptions{
		PluginArgs: []string{"proto_version '2'", "publication_names '" + st.Publication + "'"},
	})
	if err != nil {
		return fmt.Errorf("start replication: %w", err)
	}
	r.Log.Info("recording", "database", server.Database, "slot", st.Slot, "from", st.Confirmed)

	s := &session{conn: conn, store: store, state: st, log: r.Log}
	s.decoder = newDecoder(loggingSink{store, r.Log})
	return s.stream(ctx)
}

// preflight logs every setup problem and verifies that the slot is still the
// one the receipts came from. Setup problems don't stop recording: refusing
// to record would only lose more changes, and undo check reports them too.
func (r *Recorder) preflight(ctx context.Context, st receipt.State) error {
	conn, err := db.Connect(ctx, r.Config)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	settings, findings, err := setup.Check(ctx, conn)
	if err != nil {
		return err
	}
	for _, f := range findings {
		if f.Severity == setup.Problem {
			r.Log.Error("undo check: " + f.Message)
		} else {
			r.Log.Warn("undo check: " + f.Message)
		}
	}
	if settings.Slot != st.Slot || settings.Publication != st.Publication {
		return fmt.Errorf("the database uses slot %s and publication %s, but the receipts were recorded from %s and %s",
			settings.Slot, settings.Publication, st.Slot, st.Publication)
	}

	slot, err := setup.ReadSlot(ctx, conn, st.Slot)
	switch {
	case err != nil:
		return err
	case !slot.Exists:
		return fmt.Errorf("replication slot %s does not exist: run undo init --new-epoch", st.Slot)
	case slot.Active:
		return errSlotActive
	case slot.Confirmed > st.Confirmed:
		// Something else read the slot, or it was dropped and created again.
		// Either way, changes between the two positions never reached the
		// receipts.
		return fmt.Errorf("slot %s was moved to %s, past the receipts' %s: changes in between are missing; run undo init --new-epoch",
			st.Slot, slot.Confirmed, st.Confirmed)
	}
	return nil
}

// session is one replication connection's worth of streaming.
type session struct {
	conn    *pgconn.PgConn
	store   *receipt.Store
	decoder *decoder
	state   receipt.State
	log     *slog.Logger

	written    receipt.LSN // every transaction up to here is handled
	lastStatus time.Time
	replyNow   bool // Postgres asked for a status update
}

func (s *session) stream(ctx context.Context) error {
	s.written = s.state.Confirmed
	for {
		due := s.lastStatus.Add(heartbeat)
		if s.written > s.state.Confirmed {
			due = s.lastStatus.Add(confirmDelay)
		}
		if s.replyNow || !time.Now().Before(due) {
			if err := s.confirm(ctx); err != nil {
				return err
			}
			continue
		}

		recvCtx, cancel := context.WithDeadline(ctx, due)
		msg, err := s.conn.ReceiveMessage(recvCtx)
		cancel()
		if ctx.Err() != nil {
			// Shutting down: confirm what is complete. The status update
			// ignores contexts, so a socket deadline bounds it instead.
			s.conn.Conn().SetDeadline(time.Now().Add(5 * time.Second))
			return s.confirm(context.Background())
		}
		if pgconn.Timeout(err) {
			continue // time for a status update
		}
		if err != nil {
			return fmt.Errorf("receive from Postgres: %w", err)
		}

		switch msg := msg.(type) {
		case *pgproto3.CopyData:
			if err := s.handle(msg.Data); err != nil {
				return err
			}
		case *pgproto3.ErrorResponse:
			return pgconn.ErrorResponseToPgError(msg)
		}
	}
}

// handle processes one CopyData message: a keepalive or a piece of WAL.
func (s *session) handle(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	switch data[0] {
	case pglogrepl.PrimaryKeepaliveMessageByteID:
		k, err := pglogrepl.ParsePrimaryKeepaliveMessage(data[1:])
		if err != nil {
			return err
		}
		// Between transactions, everything up to the server's position is
		// handled: it holds no changes for this publication. Confirming it
		// lets Postgres recycle WAL while agents are idle.
		if s.decoder.txn == nil && receipt.LSN(k.ServerWALEnd) > s.written {
			s.written = receipt.LSN(k.ServerWALEnd)
		}
		s.replyNow = s.replyNow || k.ReplyRequested
	case pglogrepl.XLogDataByteID:
		x, err := pglogrepl.ParseXLogData(data[1:])
		if err != nil {
			return err
		}
		// Streaming of unfinished transactions is off, so inStream is false.
		msg, err := pglogrepl.ParseV2(x.WALData, false)
		if err != nil {
			return fmt.Errorf("parse replication message: %w", err)
		}
		end, err := s.decoder.handle(receipt.LSN(x.WALStart), msg)
		if err != nil {
			return err
		}
		if end > s.written {
			s.written = end
		}
	}
	return nil
}

// confirm makes everything written durable, then tells Postgres it can let
// go of it. The order matters: receipts first, then state.json, then the
// status update, so neither the file nor the server can get ahead of disk.
func (s *session) confirm(ctx context.Context) error {
	if s.written > s.state.Confirmed {
		if err := s.store.Sync(); err != nil {
			return err
		}
		next := s.state
		next.Confirmed = s.written
		if err := s.store.SaveState(next); err != nil {
			return err
		}
		s.state = next
	}
	lsn := pglogrepl.LSN(s.state.Confirmed)
	err := pglogrepl.SendStandbyStatusUpdate(ctx, s.conn, pglogrepl.StandbyStatusUpdate{
		WALWritePosition: lsn, WALFlushPosition: lsn, WALApplyPosition: lsn,
	})
	if err != nil {
		return fmt.Errorf("send status update: %w", err)
	}
	s.lastStatus, s.replyNow = time.Now(), false
	return nil
}

// retryable reports whether a later attempt might succeed: the connection
// dropped, the server is restarting or short of resources, the walsender was
// cancelled, or an old connection still holds the slot. Everything else,
// including any error writing receipts, stops the recorder: retrying after a
// failed fsync could confirm data that never reached the disk.
func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55006", // the slot is in use
			"57014",                   // query cancelled, e.g. pg_cancel_backend
			"57P01", "57P02", "57P03", // shutting down, crashed, starting up
			"57P05": // idle session timeout
			return true
		}
		return strings.HasPrefix(pgErr.Code, "08") || // connection errors
			strings.HasPrefix(pgErr.Code, "53") // out of memory, disk or connections
	}
	var opErr *net.OpError
	var dnsErr *net.DNSError
	return errors.Is(err, errSlotActive) || errors.As(err, &opErr) || errors.As(err, &dnsErr) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// loggingSink logs each recorded transaction.
type loggingSink struct {
	*receipt.Store
	log *slog.Logger
}

func (l loggingSink) Commit(run string, c *receipt.Commit) error {
	if err := l.Store.Commit(run, c); err != nil {
		return err
	}
	if c.Incomplete != "" {
		l.log.Error("recorded an incomplete transaction; undo will refuse this run", "run", run, "xid", c.XID, "reason", c.Incomplete)
	} else {
		l.log.Info("recorded", "run", run, "xid", c.XID, "changes", c.Changes, "commit", c.CommitLSN)
	}
	return nil
}

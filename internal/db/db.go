// Package db opens Postgres connections with the session settings agent-undo
// depends on.
//
// The recorder stores every value exactly as the server prints it, and undo
// later compares and writes values back as text. Printed values depend on
// session settings such as TimeZone and DateStyle, so every connection pins
// them to the same values.
package db

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EnvVar names the environment variable read when no connection string is
// given on the command line. It is deliberately not DATABASE_URL, which an
// agent's own environment may point at the agent's login.
const EnvVar = "AGENT_UNDO_DB"

// pinned holds the settings that decide how values are printed as text.
var pinned = map[string]string{
	"DateStyle":          "ISO, MDY",
	"IntervalStyle":      "postgres",
	"TimeZone":           "UTC",
	"extra_float_digits": "3", // print floats exactly
	"bytea_output":       "hex",
	"client_encoding":    "UTF8",
	"lc_monetary":        "C",
	"xmloption":          "content",
	"search_path":        "", // every query names its schema
}

// Config parses a connection string and pins agent-undo's session settings.
// An empty string reads PGHOST, PGUSER and the other PG* variables, the
// service file and ~/.pgpass, exactly like psql.
func Config(connString string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(connString)
	if err != nil {
		// pgx's message can echo parts of the connection string, so keep only
		// the underlying cause.
		var pce *pgconn.ParseConfigError
		if errors.As(err, &pce) && pce.Unwrap() != nil {
			return nil, fmt.Errorf("invalid connection string: %w", pce.Unwrap())
		}
		return nil, errors.New("invalid connection string")
	}

	for key := range cfg.RuntimeParams {
		if _, ok := pinned[canonical(key)]; ok {
			delete(cfg.RuntimeParams, key) // the user's spelling might differ in case
		}
	}
	for key, value := range pinned {
		cfg.RuntimeParams[key] = value
	}
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = "agent-undo"
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	return cfg, nil
}

// canonical maps a setting name to its spelling in pinned.
func canonical(key string) string {
	for name := range pinned {
		if strings.EqualFold(name, key) {
			return name
		}
	}
	return key
}

// Connect opens a normal SQL connection.
func Connect(ctx context.Context, cfg *pgx.ConnConfig) (*pgx.Conn, error) {
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", describe(cfg), err)
	}
	return conn, nil
}

// ConnectReplication opens a logical replication connection to the same
// database. Before streaming starts it also accepts simple SQL queries.
func ConnectReplication(ctx context.Context, cfg *pgx.ConnConfig) (*pgconn.PgConn, error) {
	replCfg := cfg.Config.Copy()
	replCfg.RuntimeParams["replication"] = "database"
	conn, err := pgconn.ConnectConfig(ctx, replCfg)
	if err != nil {
		return nil, fmt.Errorf("open replication connection to %s: %w", describe(cfg), err)
	}
	return conn, nil
}

// describe names a connection for logs and errors without its password.
func describe(cfg *pgx.ConnConfig) string {
	return fmt.Sprintf("%s@%s:%d/%s", cfg.User, cfg.Host, cfg.Port, cfg.Database)
}

// TLSWarning returns a warning when any host the connection may use is on
// another machine and the server's certificate and name are not both
// verified. pgx's default (sslmode=prefer) encrypts without verifying and may
// fall back to plain text; only sslmode=verify-full checks the name.
func TLSWarning(cfg *pgx.ConnConfig) string {
	weak := func(host string, t *tls.Config) bool {
		return !isLocal(host) && (t == nil || t.InsecureSkipVerify)
	}
	if weak(cfg.Host, cfg.TLSConfig) {
		return fmt.Sprintf("the connection to %s does not verify the server; add sslmode=verify-full", cfg.Host)
	}
	for _, fallback := range cfg.Fallbacks { // other hosts, or plain text for the same host
		if weak(fallback.Host, fallback.TLSConfig) {
			return fmt.Sprintf("the connection to %s does not verify the server; add sslmode=verify-full", fallback.Host)
		}
	}
	return ""
}

func isLocal(host string) bool {
	if strings.HasPrefix(host, "/") || host == "localhost" {
		return true // a Unix socket, or this machine
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

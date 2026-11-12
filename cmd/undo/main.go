// Command undo records what AI agents change in a Postgres database, and
// reverses one agent run's changes while keeping everyone else's later edits.
//
// Usage:
//
//	undo <command> [flags] [RUN]
//
// Commands:
//
//	init      set up recording for a database
//	check     check that recording covers everything the agent can write
//	record    write each agent run's changes to its receipt (runs until stopped)
//	show      print what a run changed
//	preview   show what undo would restore, flag or refuse, without changing anything
//	apply     undo a run exactly as previewed
//	teardown  remove agent-undo from a database
//	version   print the version
//
// Flags come before the run ID, for example: undo apply --plan 3f9a2c1d run-42.
//
// The database is the one named by --db, else by AGENT_UNDO_DB, else by the
// PG* environment variables and ~/.pgpass, as with psql.
//
// Exit codes: 0 success, 1 error, 2 bad usage, 3 something needs a person
// (undo flagged or refused changes).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/pu-suo/agent-undo/internal/db"
)

type command struct {
	name    string
	args    string // the positional arguments, for usage text
	summary string
	run     func(ctx context.Context, a *app, args []string) error
}

var commands = []command{
	{"init", "", "set up recording for a database", runInit},
	{"check", "", "check that recording covers everything the agent can write", runCheck},
	{"record", "", "write each agent run's changes to its receipt (runs until stopped)", runRecord},
	{"show", "RUN", "print what a run changed", runShow},
	{"preview", "RUN", "show what undo would do, without changing anything", runPreview},
	{"apply", "RUN", "undo a run exactly as previewed", runApply},
	{"teardown", "", "remove agent-undo from a database", runTeardown},
	{"version", "", "print the version", runVersion},
}

// errNeedsPerson means the command worked but left something for a person to
// decide, such as a flagged change. It exits with code 3.
var errNeedsPerson = errors.New("some changes need a person")

// usageError is a mistake in the command line. It exits with code 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// errFlags means the flag package rejected the arguments and has already
// printed why. It exits with code 2.
var errFlags = errors.New("invalid flags")

func main() {
	// Ctrl+C, and the SIGTERM that docker, systemd and Kubernetes send, stop
	// a command cleanly. After the first signal, stop restores the default
	// handling, so a second Ctrl+C kills the process at once.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

// run is main without the process-wide parts, so tests can call it.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		printUsage(stdout)
		return 0
	}
	var cmd *command
	for i := range commands {
		if commands[i].name == args[0] {
			cmd = &commands[i]
		}
	}
	if cmd == nil {
		fmt.Fprintf(stderr, "undo: unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}

	a := &app{
		stdout: stdout,
		stderr: stderr,
		getenv: getenv,
		log:    slog.New(slog.NewTextHandler(stderr, nil)),
	}
	err := cmd.run(ctx, a, args[1:])
	var usage usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errNeedsPerson):
		return 3
	case errors.Is(err, errFlags):
		return 2
	case errors.As(err, &usage):
		fmt.Fprintf(stderr, "undo %s: %v\nusage: undo %s [flags] %s\n", cmd.name, err, cmd.name, cmd.args)
		return 2
	default:
		fmt.Fprintf(stderr, "undo %s: %v\n", cmd.name, err)
		return 1
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "undo reverses one AI agent run's changes to a Postgres database.")
	fmt.Fprintln(w, "\nUsage: undo <command> [flags] [RUN]\n\nCommands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w, "\nRun undo <command> -h for a command's flags.")
}

// app holds what every command uses.
type app struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	log            *slog.Logger
}

// flags starts a command's flag set. Errors and help go to stderr, and
// parsing returns errors instead of exiting.
func (a *app) flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("undo "+name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	return fs
}

// parse parses flags and checks the number of positional arguments.
func parse(fs *flag.FlagSet, args []string, want int) ([]string, error) {
	if err := parseFlags(fs, args); err != nil {
		return nil, err
	}
	if fs.NArg() != want {
		return nil, usageError{fmt.Sprintf("want %d argument(s), got %d (flags go before the run ID)", want, fs.NArg())}
	}
	return fs.Args(), nil
}

// parseFlags parses flags. -h returns flag.ErrHelp; other mistakes return
// errFlags, because the flag package has already printed them.
func parseFlags(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return err
	}
	return errFlags
}

func dbFlag(fs *flag.FlagSet) *string {
	return fs.String("db", "", "connection string (default: $"+db.EnvVar+", then the PG* variables)")
}

func receiptsFlag(fs *flag.FlagSet) *string {
	return fs.String("receipts", "receipts", "receipts directory")
}

// config builds the connection settings from --db or the environment.
func (a *app) config(dbURL string) (*pgx.ConnConfig, error) {
	if dbURL == "" {
		dbURL = a.getenv(db.EnvVar)
	}
	cfg, err := db.Config(dbURL)
	if err != nil {
		return nil, err
	}
	if warning := db.TLSWarning(cfg); warning != "" {
		a.log.Warn(warning)
	}
	return cfg, nil
}

// connect opens a SQL connection from --db or the environment.
func (a *app) connect(ctx context.Context, dbURL string) (*pgx.Conn, *pgx.ConnConfig, error) {
	cfg, err := a.config(dbURL)
	if err != nil {
		return nil, nil, err
	}
	conn, err := db.Connect(ctx, cfg)
	return conn, cfg, err
}

// printable replaces characters that could control a terminal. Table names,
// column names and messages can contain text an agent chose.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsGraphic(r) {
			return r
		}
		return '?'
	}, s)
}

func runVersion(_ context.Context, a *app, args []string) error {
	if _, err := parse(a.flags("version"), args, 0); err != nil {
		return err
	}
	version, revision := "(unknown)", ""
	if info, ok := debug.ReadBuildInfo(); ok {
		version = info.Main.Version
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				revision = s.Value
			}
		}
	}
	fmt.Fprintln(a.stdout, "undo", version, revision)
	return nil
}

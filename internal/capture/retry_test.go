package capture

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRetryable(t *testing.T) {
	tests := map[string]struct {
		err  error
		want bool
	}{
		"connection reset":       {&net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, true},
		"connection closed":      {fmt.Errorf("receive from Postgres: %w", io.ErrUnexpectedEOF), true},
		"walsender cancelled":    {&pgconn.PgError{Code: "57014"}, true},
		"server shutting down":   {&pgconn.PgError{Code: "57P01"}, true},
		"server out of memory":   {&pgconn.PgError{Code: "53200"}, true},
		"slot in use":            {fmt.Errorf("preflight: %w", errSlotActive), true},
		"wrong password":         {&pgconn.PgError{Code: "28P01"}, false},
		"publication missing":    {&pgconn.PgError{Code: "42704"}, false},
		"slot invalidated":       {&pgconn.PgError{Code: "55000"}, false},
		"receipt fsync failed":   {fmt.Errorf("sync receipt: %w", &fs.PathError{Op: "sync", Path: "r", Err: syscall.EIO}), false},
		"receipts disk full":     {&fs.PathError{Op: "write", Path: "r", Err: syscall.ENOSPC}, false},
		"receipts not writable":  {&fs.PathError{Op: "open", Path: "r", Err: syscall.EACCES}, false},
		"too many files open":    {&fs.PathError{Op: "open", Path: "r", Err: syscall.EMFILE}, false},
		"unexpected replication": {errors.New("unexpected X message"), false},
	}
	for name, tt := range tests {
		if got := retryable(tt.err); got != tt.want {
			t.Errorf("%s: retryable = %v, want %v", name, got, tt.want)
		}
	}
}

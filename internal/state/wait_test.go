package state

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// A database that isn't up yet is waited for, within the limit; one that
// answers no is not.
func TestWaitForTheDatabase(t *testing.T) {
	var told []string
	start := time.Now()
	_, _, err := openPostgres("postgres://cobs@127.0.0.1:1/cobs?sslmode=disable&connect_timeout=1", Options{
		Wait: 1500 * time.Millisecond,
		Log:  func(f string, a ...any) { told = append(told, fmt.Sprintf(f, a...)) },
	})
	took := time.Since(start)
	if err == nil || took < time.Second || took > 10*time.Second {
		t.Fatalf("err %v after %v: want a failure after about the wait", err, took)
	}
	if len(told) != 1 {
		t.Fatalf("told %q: want one line while waiting", told)
	}
	start = time.Now()
	if _, _, err := openPostgres("postgres://cobs@127.0.0.1:1/cobs?sslmode=disable&connect_timeout=1", Options{}); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("without a wait: %v after %v", err, time.Since(start))
	}
	if !notUpYet(errors.New("dial tcp: connection refused")) || !notUpYet(fmt.Errorf("x: %w", &pgconn.PgError{Code: "57P03"})) {
		t.Fatal("a starting database counted as final")
	}
	if notUpYet(&pgconn.PgError{Code: "28P01"}) || notUpYet(&pgconn.PgError{Code: "3D000"}) {
		t.Fatal("a refused password or missing database waited for")
	}
}

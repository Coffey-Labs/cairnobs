package mapepire

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeServer(t *testing.T, answer func(string) ([]Row, string)) (*Fake, string, *tls.Config) {
	t.Helper()
	f := &Fake{User: "CAIRNOBS", Password: "s3cret", Answer: answer}
	srv := httptest.NewTLSServer(f.Handler())
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return f, strings.TrimPrefix(srv.URL, "https://"), &tls.Config{RootCAs: pool}
}

func TestQuery(t *testing.T) {
	var many []Row
	for i := range 1234 {
		many = append(many, Row{"N": float64(i)})
	}
	f, addr, tc := fakeServer(t, func(sql string) ([]Row, string) {
		switch {
		case strings.Contains(sql, "ENV_SYS_INFO"):
			return []Row{{"OS_NAME": "IBM i", "OS_VERSION": "7", "OS_RELEASE": "5"}}, ""
		case strings.Contains(sql, "MANY"):
			return many, ""
		}
		return nil, "[SQL0204] NOPE in QSYS2 type *FILE not found."
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := Dial(ctx, addr, "CAIRNOBS", "s3cret", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Job != "123456/QUSER/QZDASOINIT" {
		t.Errorf("job %q", c.Job)
	}
	rows, err := c.Query(ctx, "SELECT OS_NAME, OS_VERSION, OS_RELEASE FROM SYSIBMADM.ENV_SYS_INFO")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["OS_RELEASE"] != "5" {
		t.Errorf("rows %v", rows)
	}
	rows, err = c.Query(ctx, "SELECT N FROM MANY")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1234 || rows[1233]["N"] != float64(1233) {
		t.Errorf("paged rows: %d, last %v", len(rows), rows[len(rows)-1])
	}
	_, err = c.Query(ctx, "SELECT * FROM QSYS2.NOPE")
	var me *Error
	if !errors.As(err, &me) || me.SQLState != "42704" || !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("a missing view: %v", err)
	}
	// Basic query mode: nothing but a SELECT.
	if _, err = c.Query(ctx, "DELETE FROM QSYS2.ANYTHING"); !errors.As(err, &me) || me.SQLState != "42501" {
		t.Errorf("a DELETE: %v", err)
	}
	// The connection still answers after a refusal.
	if _, err = c.Query(ctx, "SELECT OS_NAME FROM SYSIBMADM.ENV_SYS_INFO"); err != nil {
		t.Error(err)
	}
	if len(f.Queries) != 5 {
		t.Errorf("queries %q", f.Queries)
	}
}

func TestTooManyRows(t *testing.T) {
	var many []Row
	for i := range MaxRows + 10 {
		many = append(many, Row{"N": float64(i)})
	}
	_, addr, tc := fakeServer(t, func(string) ([]Row, string) { return many, "" })
	ctx := context.Background()
	c, err := Dial(ctx, addr, "CAIRNOBS", "s3cret", tc)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Query(ctx, "SELECT N FROM MANY"); err == nil || !strings.Contains(err.Error(), fmt.Sprint(MaxRows)) {
		t.Errorf("err %v", err)
	}
}

func TestSignInRefused(t *testing.T) {
	_, addr, tc := fakeServer(t, func(string) ([]Row, string) { return nil, "" })
	_, err := Dial(context.Background(), addr, "CAIRNOBS", "wrong", tc)
	if err == nil {
		t.Fatal("signed in with the wrong password")
	}
}

func TestUntrustedCertificate(t *testing.T) {
	_, addr, _ := fakeServer(t, func(string) ([]Row, string) { return nil, "" })
	_, err := Dial(context.Background(), addr, "CAIRNOBS", "s3cret", &tls.Config{})
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("err %v", err)
	}
}

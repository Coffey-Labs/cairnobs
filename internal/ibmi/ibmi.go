// Package ibmi reads an IBM i partition through Mapepire (docs/ibmi.md),
// as a user profile with no special authorities, read only: IBM's SQL
// services views, every minute, and nothing that isn't a SELECT.
package ibmi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/mapepire"
	"github.com/cairnobs/cairnobs/internal/tlspin"
)

// Config is one partition in the agent's ibmi: section.
type Config struct {
	// Name is what the partition is kept under, beside the agent's name
	// ("mgmt1:prod1"): default its host name, lowercased.
	Name    string `yaml:"name"`
	Address string `yaml:"address"` // prod1.example.com, or with :port (8076)
	User    string `yaml:"user"`    // the profile: CAIRNOBS
	// PasswordFile holds the profile's password, alone in the file.
	PasswordFile string `yaml:"password_file"`
	CAFile       string `yaml:"ca_file"` // Mapepire's CA, PEM; else the system's
	// Fingerprint pins Mapepire's certificate (SHA-256, hex, colons or
	// not) when it's the self-signed one it makes for itself.
	Fingerprint string        `yaml:"fingerprint"`
	Every       time.Duration `yaml:"every"` // between reads: 60s
}

// Defaults fills in what's left out.
func (c *Config) Defaults() {
	if c.Every == 0 {
		c.Every = time.Minute
	}
	if c.Name == "" {
		host, _, err := net.SplitHostPort(c.Address)
		if err != nil {
			host = c.Address
		}
		c.Name = strings.ToLower(host)
	}
}

// Client reads one partition. It keeps its Mapepire connection between
// reads, and connects again when it's lost. One goroutine uses it.
type Client struct {
	cfg      Config
	password string
	dial     func(ctx context.Context) (querier, error)
	conn     querier
	last     *cpuMark // the last read's CPU columns, for the minute between
}

// querier is a Mapepire connection, or a test's stand-in.
type querier interface {
	Query(ctx context.Context, sql string) ([]mapepire.Row, error)
	Close() error
}

// New checks cfg and reads the password and the CA; it doesn't connect yet.
func New(cfg Config) (*Client, error) {
	cfg.Defaults()
	if cfg.Address == "" {
		return nil, errors.New("ibmi: address is required")
	}
	if cfg.User == "" {
		return nil, errors.New("ibmi: user is required")
	}
	b, err := os.ReadFile(cfg.PasswordFile)
	if err != nil {
		return nil, fmt.Errorf("ibmi: the password: %w", err)
	}
	pw := strings.TrimRight(string(b), "\r\n")
	if pw == "" {
		return nil, errors.New("ibmi: the password file is empty")
	}
	tc, err := tlspin.Config(cfg.CAFile, cfg.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("ibmi: %w", err)
	}
	c := &Client{cfg: cfg, password: pw}
	c.dial = func(ctx context.Context) (querier, error) {
		return mapepire.Dial(ctx, cfg.Address, cfg.User, pw, tc)
	}
	return c, nil
}

// Name is what the partition is kept under.
func (c *Client) Name() string { return c.cfg.Name }

// query runs one SELECT, connecting first when there's no connection. A
// connection that fails is dropped, so the next read connects again; Db2
// refusing the query leaves it be.
func (c *Client) query(ctx context.Context, sql string) ([]mapepire.Row, error) {
	if c.conn == nil {
		q, err := c.dial(ctx)
		if err != nil {
			return nil, err
		}
		c.conn, c.last = q, nil // a new Db2 job: its statistics start again
	}
	rows, err := c.conn.Query(ctx, sql)
	var me *mapepire.Error
	if err != nil && !errors.As(err, &me) {
		c.conn.Close()
		c.conn = nil
	}
	return rows, err
}

// Close ends the connection.
func (c *Client) Close() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// The column helpers: Mapepire sends DECIMAL and BIGINT as JSON numbers,
// CHAR padded with blanks, and NULL as null.

func str(r mapepire.Row, k string) string {
	switch v := r[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// num is a number column; ok is false when it's null.
func num(r mapepire.Row, k string) (float64, bool) {
	switch v := r[k].(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	}
	return 0, false
}

func numOr0(r mapepire.Row, k string) float64 { f, _ := num(r, k); return f }

func round1(f float64) float64 { return math.Round(f*10) / 10 }

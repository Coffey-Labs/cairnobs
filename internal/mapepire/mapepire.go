// Package mapepire asks Db2 for i through Mapepire, IBM's database server
// for IBM i (docs/ibmi.md, decision 1): SQL over a secure WebSocket, one
// JSON request a message, each answer carrying the request's id. Only
// what reading needs is spoken: connect, sql, sqlmore and sqlclose.
//
// Every connection asks for "access=read only", which puts Mapepire in its
// basic query mode: it refuses anything that isn't a query of tables and
// views, and its JDBC connection is read only.
package mapepire

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"golang.org/x/net/websocket"
)

// DefaultPort is Mapepire's own.
const DefaultPort = "8076"

// MaxRows is the most rows one query reads: what's read is a partition's
// status, never a table's contents.
const MaxRows = 5000

// Error is Mapepire or Db2 refusing a request.
type Error struct {
	Msg      string
	SQLState string
	SQLCode  int
}

func (e *Error) Error() string {
	if e.SQLState != "" {
		return fmt.Sprintf("%s (SQLSTATE %s, SQLCODE %d)", e.Msg, e.SQLState, e.SQLCode)
	}
	return e.Msg
}

// Conn is one signed-in Mapepire connection: one Db2 job on the IBM i.
// One goroutine uses it.
type Conn struct {
	ws  *websocket.Conn
	n   int
	Job string // the Db2 job serving it: 123456/QUSER/QZDASOINIT
}

// Dial connects to addr (host or host:port) and signs in as user, read
// only. tc says which certificate to trust; its ServerName is set from
// addr when empty.
func Dial(ctx context.Context, addr, user, password string, tc *tls.Config) (*Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, DefaultPort
	}
	hp := net.JoinHostPort(host, port)
	cfg, err := websocket.NewConfig("wss://"+hp+"/db/", "https://"+hp)
	if err != nil {
		return nil, err
	}
	tc = tc.Clone()
	if tc.ServerName == "" {
		tc.ServerName = host
	}
	cfg.TlsConfig = tc
	cfg.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+password)))
	cfg.Dialer = &net.Dialer{Timeout: 15 * time.Second}
	ws, err := cfg.DialContext(ctx)
	if err != nil {
		var de *websocket.DialError
		if errors.As(err, &de) && de.Err != nil {
			err = de.Err // "bad status" when the sign-in is refused
		}
		return nil, fmt.Errorf("mapepire: connecting to %s: %w", hp, err)
	}
	ws.MaxPayloadBytes = 16 << 20
	c := &Conn{ws: ws}
	var r struct {
		Job string `json:"job"`
	}
	if err := c.do(ctx, map[string]any{"type": "connect", "props": "access=read only", "application": "Cairn OBS"}, &r); err != nil {
		ws.Close()
		return nil, err
	}
	c.Job = r.Job
	return c, nil
}

// Close ends the connection, and with it the Db2 job.
func (c *Conn) Close() error { return c.ws.Close() }

// Row is one row, by column name.
type Row map[string]any

// Query runs one SELECT and reads all its rows, up to MaxRows; more is an
// error, and the cursor is closed.
func (c *Conn) Query(ctx context.Context, sql string) ([]Row, error) {
	var r struct {
		ID     string `json:"id"`
		Data   []Row  `json:"data"`
		IsDone bool   `json:"is_done"`
	}
	if err := c.do(ctx, map[string]any{"type": "sql", "sql": sql, "rows": 500}, &r); err != nil {
		return nil, err
	}
	rows := r.Data
	for !r.IsDone {
		if len(rows) >= MaxRows {
			c.do(ctx, map[string]any{"type": "sqlclose", "cont_id": r.ID}, nil)
			return nil, fmt.Errorf("mapepire: more than %d rows", MaxRows)
		}
		id := r.ID
		r.Data, r.IsDone = nil, false
		if err := c.do(ctx, map[string]any{"type": "sqlmore", "cont_id": id, "rows": 500}, &r); err != nil {
			return nil, err
		}
		r.ID = id
		rows = append(rows, r.Data...)
	}
	return rows, nil
}

// do sends one request and reads until its answer comes: anything
// answering an earlier request is skipped. out may be nil.
func (c *Conn) do(ctx context.Context, req map[string]any, out any) error {
	c.n++
	id := "c" + strconv.Itoa(c.n)
	req["id"] = id
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(time.Minute)
	}
	c.ws.SetDeadline(dl)
	if err := websocket.JSON.Send(c.ws, req); err != nil {
		return fmt.Errorf("mapepire: %w", err)
	}
	for {
		var raw json.RawMessage
		if err := websocket.JSON.Receive(c.ws, &raw); err != nil {
			return fmt.Errorf("mapepire: %w", err)
		}
		var head struct {
			ID       string `json:"id"`
			Success  bool   `json:"success"`
			Error    string `json:"error"`
			SQLState string `json:"sql_state"`
			SQLRC    int    `json:"sql_rc"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return fmt.Errorf("mapepire: an answer that isn't JSON: %w", err)
		}
		if head.ID != id {
			continue
		}
		if !head.Success {
			msg := head.Error
			if msg == "" {
				msg = "refused, with no reason given"
			}
			return &Error{Msg: msg, SQLState: head.SQLState, SQLCode: head.SQLRC}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("mapepire: %w", err)
		}
		return nil
	}
}

package mapepire

import (
	"encoding/base64"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/websocket"
)

// Fake answers as a Mapepire server would, for tests and the demo
// simulator: each query's rows from Answer, by its SQL. It checks the
// sign-in and that every connection asked to be read only, and refuses
// what isn't a SELECT, as basic query mode does.
type Fake struct {
	User, Password string
	// Answer gives a query's rows, or an error message as Db2 would say it.
	Answer func(sql string) ([]Row, string)

	mu      sync.Mutex
	Queries []string // every SQL asked, in order
}

// Handler serves /db/.
func (f *Fake) Handler() http.Handler {
	ws := websocket.Server{Handler: f.serve, Handshake: func(*websocket.Config, *http.Request) error { return nil }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(f.User+":"+f.Password))
		if r.URL.Path != "/db/" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != want {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		ws.ServeHTTP(w, r)
	})
}

func (f *Fake) serve(c *websocket.Conn) {
	defer c.Close()
	readOnly := false
	open := map[string][]Row{} // cursors not yet read to the end
	for {
		var req map[string]any
		if err := websocket.JSON.Receive(c, &req); err != nil {
			return
		}
		id, _ := req["id"].(string)
		out := map[string]any{"id": id, "success": true, "execution_time": 1}
		fail := func(msg, state string, rc int) {
			out["success"], out["error"] = false, msg
			if state != "" {
				out["sql_state"], out["sql_rc"] = state, rc
			}
		}
		rows := func(n any) int {
			if f, ok := n.(float64); ok && f > 0 {
				return int(f)
			}
			return 1000
		}
		switch req["type"] {
		case "connect":
			readOnly = req["props"] == "access=read only"
			out["job"] = "123456/QUSER/QZDASOINIT"
		case "sql":
			sql, _ := req["sql"].(string)
			f.mu.Lock()
			f.Queries = append(f.Queries, sql)
			f.mu.Unlock()
			if !readOnly || !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(sql)), "SELECT") {
				fail("[SQL0551] Not authorized: basic query mode", "42501", -551)
				break
			}
			data, msg := f.Answer(sql)
			if msg != "" {
				fail(msg, "42704", -204)
				break
			}
			n := rows(req["rows"])
			if len(data) > n {
				open[id] = data[n:]
				data = data[:n]
			}
			if data == nil {
				data = []Row{}
			}
			out["has_results"], out["data"], out["is_done"] = true, data, open[id] == nil
		case "sqlmore":
			cont, _ := req["cont_id"].(string)
			rest, ok := open[cont]
			if !ok {
				fail("invalid correlation ID", "", 0)
				break
			}
			n := rows(req["rows"])
			data := rest
			if len(rest) > n {
				open[cont], data = rest[n:], rest[:n]
			} else {
				delete(open, cont)
			}
			out["data"], out["is_done"] = data, open[cont] == nil
		case "sqlclose":
			cont, _ := req["cont_id"].(string)
			delete(open, cont)
		default:
			fail("unknown request type", "", 0)
		}
		if err := websocket.JSON.Send(c, out); err != nil {
			return
		}
	}
}

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOnlyLocalAddresses(t *testing.T) {
	for _, u := range []string{"https://example.com/", "http://8.8.8.8/", "ftp://127.0.0.1/", "http://user:pw@127.0.0.1/", "http://metadata.google.internal/"} {
		if (Spec{ID: "x", URL: u}).Check() == nil {
			t.Errorf("allowed %s", u)
		}
	}
	for _, u := range []string{"http://127.0.0.1:8222/varz", "http://10.0.0.1:8008/cluster", "http://localhost:3903/health"} {
		if err := (Spec{ID: "x", URL: u}).Check(); err != nil {
			t.Errorf("refused %s: %v", u, err)
		}
	}
}

func TestGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("Garage is degraded"))
	}))
	defer srv.Close()
	f := Get(context.Background(), Spec{ID: "garage", URL: srv.URL + "/health"})
	if f.Status != 503 || f.Body != "Garage is degraded" || f.Error != "" {
		t.Fatalf("%+v", f)
	}
	if f := Get(context.Background(), Spec{ID: "x", URL: "http://127.0.0.1:1/"}); f.Error == "" {
		t.Fatal("no error for a closed port")
	}
}

func TestKeep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("# HELP x\netcd_server_has_leader 1\ngo_goroutines 99\netcd_mvcc_db_total_size_in_bytes 1.1e+06\n"))
	}))
	defer srv.Close()
	f := Get(context.Background(), Spec{ID: "etcd", URL: srv.URL, Keep: []string{"etcd_server_has_leader", "etcd_mvcc_db_total_size_in_bytes"}})
	if f.Body != "etcd_server_has_leader 1\netcd_mvcc_db_total_size_in_bytes 1.1e+06\n" {
		t.Fatalf("%q", f.Body)
	}
}

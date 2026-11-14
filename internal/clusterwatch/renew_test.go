package clusterwatch

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cairnobs/cairnobs/internal/kube"
)

// Renew sets Issuing as cmctl renew does, keeps the other conditions,
// and says why when it can't.
func TestRenew(t *testing.T) {
	var issuing, patchCode = "False", 200
	var patched map[string]any
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const path = "/apis/cert-manager.io/v1/namespaces/shop/certificates/web"
		switch {
		case r.Method == "GET" && r.URL.Path == path:
			_, _ = io.WriteString(w, `{"metadata":{"name":"web","generation":3,"resourceVersion":"77"},"status":{"conditions":[
				{"type":"Ready","status":"True","reason":"Ready","message":"up to date","lastTransitionTime":"2026-11-01T00:00:00Z","observedGeneration":3},
				{"type":"Issuing","status":"`+issuing+`","reason":"Old","lastTransitionTime":"2026-10-01T00:00:00Z"}]}}`)
		case r.Method == "PATCH" && r.URL.Path == path+"/status":
			if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
				t.Errorf("content type %s", ct)
			}
			if patchCode != 200 {
				w.WriteHeader(patchCode)
				_, _ = io.WriteString(w, `{"kind":"Status","code":`+map[int]string{403: "403", 409: "409"}[patchCode]+`}`)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&patched)
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "/namespaces/denied/"):
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"kind":"Status","code":403}`)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"kind":"Status","code":404}`)
		}
	}))
	defer ts.Close()
	tok := t.TempDir() + "/token"
	_ = writeFile(tok, "sa-token\n")
	kc := kube.New(ts.URL, ts.Client().Transport.(*http.Transport).TLSClientConfig, tok)
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

	if err := Renew(t.Context(), kc, "shop", "web", now); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(patched)
	want := `{"metadata":{"resourceVersion":"77"},"status":{"conditions":[` +
		`{"lastTransitionTime":"2026-11-01T00:00:00Z","message":"up to date","observedGeneration":3,"reason":"Ready","status":"True","type":"Ready"},` +
		`{"lastTransitionTime":"2026-11-14T09:00:00Z","message":"Certificate re-issuance manually triggered (Cairn OBS)","observedGeneration":3,"reason":"ManuallyTriggered","status":"True","type":"Issuing"}]}}`
	if string(b) != want {
		t.Errorf("patch:\n%s\nwant\n%s", b, want)
	}

	issuing = "True"
	if err := Renew(t.Context(), kc, "shop", "web", now); err == nil || !strings.Contains(err.Error(), "already renewing shop/web") {
		t.Errorf("already issuing: %v", err)
	}
	issuing = "False"
	patchCode = 403
	if err := Renew(t.Context(), kc, "shop", "web", now); err == nil || !strings.Contains(err.Error(), "apply cert-renew.yaml") {
		t.Errorf("no role: %v", err)
	}
	patchCode = 409
	if err := Renew(t.Context(), kc, "shop", "web", now); err == nil || !strings.Contains(err.Error(), "changed while asking") {
		t.Errorf("conflict: %v", err)
	}
	if err := Renew(t.Context(), kc, "denied", "web", now); err == nil || !strings.Contains(err.Error(), "may not read cert-manager's Certificates") {
		t.Errorf("can't read: %v", err)
	}
	if err := Renew(t.Context(), kc, "shop", "gone", now); err == nil || !strings.Contains(err.Error(), "no Certificate shop/gone") {
		t.Errorf("missing: %v", err)
	}
	for _, bad := range [][2]string{{"Shop", "web"}, {"shop", "../x"}, {"shop", ""}, {"a/b", "web"}} {
		if err := Renew(t.Context(), kc, bad[0], bad[1], now); err == nil || !strings.Contains(err.Error(), "not a Certificate's") {
			t.Errorf("%v: %v", bad, err)
		}
	}
}

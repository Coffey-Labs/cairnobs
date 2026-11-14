package akamai

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Creds are an Akamai API client's four values, from an .edgerc section.
type Creds struct {
	Host         string // akab-….luna.akamaiapis.net
	ClientToken  string
	ClientSecret string
	AccessToken  string
	MaxBody      int  // how much of a POST body is hashed; default 131072
	Plain        bool // http, to a stand-in on loopback
}

// ReadEdgerc reads section of an .edgerc file, the format every Akamai
// tool uses:
//
//	[default]
//	host = akab-….luna.akamaiapis.net
//	client_token = akab-…
//	client_secret = …
//	access_token = akab-…
func ReadEdgerc(path, section string) (Creds, error) {
	f, err := os.Open(path)
	if err != nil {
		return Creds{}, err
	}
	defer f.Close()
	var c Creds
	in, found := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";"):
			continue
		case strings.HasPrefix(l, "[") && strings.HasSuffix(l, "]"):
			in = strings.TrimSpace(l[1:len(l)-1]) == section
			found = found || in
			continue
		case !in:
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "host":
			c.Host = strings.TrimSuffix(strings.TrimPrefix(v, "https://"), "/")
			if h, ok := strings.CutPrefix(c.Host, "http://"); ok {
				// A stand-in on this machine (dev/fakeakamai); nothing
				// else is spoken to in plain text.
				if host, _, err := net.SplitHostPort(h); err != nil || (host != "127.0.0.1" && host != "localhost" && host != "::1") {
					return Creds{}, fmt.Errorf("%s [%s]: host must be https, or http on loopback for a stand-in", path, section)
				}
				c.Plain = true
				c.Host = h
			}
		case "client_token":
			c.ClientToken = v
		case "client_secret":
			c.ClientSecret = v
		case "access_token":
			c.AccessToken = v
		case "max-body", "max_body":
			c.MaxBody, _ = strconv.Atoi(v)
		}
	}
	if err := sc.Err(); err != nil {
		return Creds{}, err
	}
	switch {
	case !found:
		return Creds{}, fmt.Errorf("%s has no [%s] section", path, section)
	case c.Host == "" || c.ClientToken == "" || c.ClientSecret == "" || c.AccessToken == "":
		return Creds{}, fmt.Errorf("%s [%s] needs host, client_token, client_secret and access_token", path, section)
	}
	return c, nil
}

// edgegridTime is the timestamp's layout.
const edgegridTime = "20060102T15:04:05+0000"

// Sign sets r's EdgeGrid Authorization header (EG1-HMAC-SHA256). body is
// a POST's (the one query the Reporting API takes that way); no headers
// are signed.
func (c Creds) Sign(r *http.Request, body []byte, now time.Time) {
	var n [16]byte
	_, _ = rand.Read(n[:])
	nonce := hex.EncodeToString(n[:])
	r.Header.Set("Authorization", c.authorization(r, body, now.UTC().Format(edgegridTime), nonce[:8]+"-"+nonce[8:12]+"-"+nonce[12:16]+"-"+nonce[16:20]+"-"+nonce[20:]))
}

func (c Creds) authorization(r *http.Request, body []byte, ts, nonce string) string {
	auth := "EG1-HMAC-SHA256 client_token=" + c.ClientToken + ";access_token=" + c.AccessToken + ";timestamp=" + ts + ";nonce=" + nonce + ";"
	path := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	hash := ""
	if r.Method == http.MethodPost && len(body) > 0 {
		max := c.MaxBody
		if max <= 0 {
			max = 131072
		}
		sum := sha256.Sum256(body[:min(len(body), max)])
		hash = base64.StdEncoding.EncodeToString(sum[:])
	}
	data := strings.Join([]string{r.Method, r.URL.Scheme, r.URL.Host, path, "", hash, auth}, "\t")
	key := mac(c.ClientSecret, ts)
	return auth + "signature=" + mac(key, data)
}

// mac is base64(HMAC-SHA256(key, msg)).
func mac(key, msg string) string {
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// Check verifies a request's signature against c: the fakes' side.
func (c Creds) Check(r *http.Request) bool {
	a := r.Header.Get("Authorization")
	var ts, nonce string
	for _, f := range strings.Split(strings.TrimPrefix(a, "EG1-HMAC-SHA256 "), ";") {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "timestamp":
			ts = v
		case "nonce":
			nonce = v
		}
	}
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	u := *r.URL
	u.Scheme, u.Host = "https", c.Host
	rr := &http.Request{Method: r.Method, URL: &u}
	return ts != "" && hmac.Equal([]byte(c.authorization(rr, body, ts, nonce)), []byte(a))
}

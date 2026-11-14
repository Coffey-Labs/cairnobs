// Package kube is the little of the Kubernetes API the cluster agent uses:
// get, list (in pages) and watch, plus patching its own Secret. It speaks
// JSON over HTTPS with the pod's service account, and decodes only what
// the caller asks for, so whole objects never sit in memory.
//
// It's not client-go: that would be most of the agent's size for four
// calls (docs/cluster-watcher.md).
package kube

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The service account's files in every pod that mounts its token.
const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// pageSize is how many objects a list asks for at a time.
const pageSize = 500

// Client talks to one cluster's API.
type Client struct {
	base      string
	http      *http.Client // for get, list and patch: with a timeout
	stream    *http.Client // for watches: no overall timeout, the context ends them
	tokenFile string
	namespace string

	mu        sync.Mutex
	token     string
	tokenRead time.Time
}

// InCluster is the client for the cluster the agent runs in, from the
// service account Kubernetes mounts.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not in a cluster: KUBERNETES_SERVICE_HOST and _PORT aren't set")
	}
	ca, err := os.ReadFile(filepath.Join(saDir, "ca.crt"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("the service account's ca.crt holds no certificate")
	}
	ns, err := os.ReadFile(filepath.Join(saDir, "namespace"))
	if err != nil {
		return nil, err
	}
	c := New("https://"+net.JoinHostPort(host, port), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, filepath.Join(saDir, "token"))
	c.namespace = strings.TrimSpace(string(ns))
	if v := os.Getenv("POD_NAMESPACE"); v != "" {
		c.namespace = v
	}
	return c, nil
}

// New is a client for base (https://host:port) with tc, authenticating
// with the bearer token in tokenFile ("" for none). Tests use it with an
// httptest server.
func New(base string, tc *tls.Config, tokenFile string) *Client {
	tr := func() *http.Transport {
		return &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true, MaxIdleConns: 4, IdleConnTimeout: time.Minute,
			ResponseHeaderTimeout: 30 * time.Second}
	}
	return &Client{base: strings.TrimRight(base, "/"), tokenFile: tokenFile,
		http:   &http.Client{Timeout: time.Minute, Transport: tr()},
		stream: &http.Client{Transport: tr()}}
}

// Namespace is the agent's own namespace.
func (c *Client) Namespace() string { return c.namespace }

// bearer is the service account token, read again every minute: the
// kubelet replaces it well before it runs out.
func (c *Client) bearer() (string, error) {
	if c.tokenFile == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.tokenRead) < time.Minute {
		return c.token, nil
	}
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		if c.token != "" {
			return c.token, nil // keep the last one; it's good for a while
		}
		return "", err
	}
	c.token, c.tokenRead = strings.TrimSpace(string(b)), time.Now()
	return c.token, nil
}

// StatusError is an answer other than 2xx: the API's Status, when it sent
// one.
type StatusError struct {
	Code    int
	Reason  string // Forbidden, NotFound, Expired...
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%d %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%d %s", e.Code, http.StatusText(e.Code))
}

// Code is the HTTP status of err, 0 when it isn't a StatusError.
func Code(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// ErrGone is a watch or list from a resourceVersion the API no longer
// has: list again.
var ErrGone = errors.New("resource version too old")

func statusError(code int, body []byte) error {
	var st struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	_ = json.Unmarshal(body, &st)
	if code == http.StatusGone || st.Code == http.StatusGone {
		return ErrGone
	}
	if st.Message == "" {
		st.Message = strings.TrimSpace(string(body))
		if len(st.Message) > 200 {
			st.Message = st.Message[:200]
		}
	}
	return &StatusError{Code: code, Reason: st.Reason, Message: st.Message}
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, q url.Values, body []byte, ctype string) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	tok, err := c.bearer()
	if err != nil {
		return nil, err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Accept", "application/json")
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, statusError(resp.StatusCode, b)
	}
	return resp, nil
}

// Get decodes the object at path into v.
func (c *Client) Get(ctx context.Context, path string, v any) error {
	resp, err := c.do(ctx, c.http, http.MethodGet, path, nil, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(v)
}

// MergePatch sends a JSON merge patch to the object at path.
func (c *Client) MergePatch(ctx context.Context, path string, patch any) error {
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, c.http, http.MethodPatch, path, nil, b, "application/merge-patch+json")
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// List calls each with every object at path (a collection, such as
// /api/v1/pods), asking for them in pages, and returns the list's
// resourceVersion to watch from.
func (c *Client) List(ctx context.Context, path string, each func(raw json.RawMessage) error) (string, error) {
	q := url.Values{"limit": {strconv.Itoa(pageSize)}}
	for {
		resp, err := c.do(ctx, c.http, http.MethodGet, path, q, nil, "")
		if err != nil {
			return "", err
		}
		var page struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
				Continue        string `json:"continue"`
			} `json:"metadata"`
			Items []json.RawMessage `json:"items"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("listing %s: %w", path, err)
		}
		for _, it := range page.Items {
			if err := each(it); err != nil {
				return "", err
			}
		}
		if page.Metadata.Continue == "" {
			return page.Metadata.ResourceVersion, nil
		}
		q.Set("continue", page.Metadata.Continue)
	}
}

// Event types in a watch.
const (
	Added    = "ADDED"
	Modified = "MODIFIED"
	Deleted  = "DELETED"
	Bookmark = "BOOKMARK"
)

// Watch calls fn with each change at path from resourceVersion rv, until
// the API ends the watch (after timeout, or sooner), the context ends, or
// fn fails. It returns the last resourceVersion seen, bookmarks included,
// to watch on from; ErrGone means list again. Bookmarks don't reach fn.
func (c *Client) Watch(ctx context.Context, path, rv string, timeout time.Duration, fn func(typ string, raw json.RawMessage) error) (string, error) {
	q := url.Values{"watch": {"1"}, "resourceVersion": {rv}, "allowWatchBookmarks": {"true"},
		"timeoutSeconds": {strconv.Itoa(int(timeout.Seconds()))}}
	resp, err := c.do(ctx, c.stream, http.MethodGet, path, q, nil, "")
	if err != nil {
		return rv, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(resp.Body, 64<<10))
	for {
		var ev struct {
			Type   string          `json:"type"`
			Object json.RawMessage `json:"object"`
		}
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return rv, ctx.Err()
			}
			return rv, err
		}
		if ev.Type == "ERROR" {
			return rv, statusError(0, ev.Object)
		}
		var meta struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(ev.Object, &meta); err == nil && meta.Metadata.ResourceVersion != "" {
			rv = meta.Metadata.ResourceVersion
		}
		if ev.Type == Bookmark {
			continue
		}
		if err := fn(ev.Type, ev.Object); err != nil {
			return rv, err
		}
	}
}

// Version is the API server's version (/version), e.g. "v1.32.2".
func (c *Client) Version(ctx context.Context) (string, error) {
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := c.Get(ctx, "/version", &v); err != nil {
		return "", err
	}
	return v.GitVersion, nil
}

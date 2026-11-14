// Package docker reads Docker's container list, and provides the read-only
// gate that the agent's root helper puts in front of the Docker socket.
//
// The agent's main process never opens /var/run/docker.sock (that socket is
// root on the host). It talks to the helper's socket instead, and the helper
// passes on only the GET requests in allowed; anything that could start,
// stop, exec or change something is refused.
package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"bytes"
	"github.com/cairnobs/cairnobs/internal/certreload"
	"github.com/cairnobs/cairnobs/internal/libvirt"
	"github.com/cairnobs/cairnobs/internal/report"
)

func unixTransport(socket string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    2,
		IdleConnTimeout: time.Minute,
	}
}

// Client asks Docker (or the helper in front of it) over a unix socket.
type Client struct{ hc *http.Client }

// NewClient talks to the Docker API on the given unix socket.
func NewClient(socket string) *Client {
	return &Client{hc: &http.Client{Transport: unixTransport(socket), Timeout: 10 * time.Second}}
}

type apiContainer struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
	Created int64             `json:"Created"`
	Ports   []struct {
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	} `json:"Ports"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Containers lists every container, running or not.
func (c *Client) Containers(ctx context.Context) ([]report.Container, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json?all=1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.Header.Get(noDockerHeader) != "" {
		return nil, ErrNoDocker
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker: %s", resp.Status)
	}
	var list []apiContainer
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	out := make([]report.Container, 0, len(list))
	for _, a := range list {
		name := a.ID
		if len(a.Names) > 0 {
			name = strings.TrimPrefix(a.Names[0], "/")
		}
		var ports []report.PortMap
		for _, p := range a.Ports {
			pm := report.PortMap{Public: p.PublicPort, Private: p.PrivatePort, Proto: p.Type}
			// Docker lists a port once per host address (0.0.0.0 and ::).
			if p.PublicPort != 0 && !slices.Contains(ports, pm) {
				ports = append(ports, pm)
			}
		}
		var ips []string
		for _, n := range a.NetworkSettings.Networks {
			if n.IPAddress != "" && !slices.Contains(ips, n.IPAddress) {
				ips = append(ips, n.IPAddress)
			}
		}
		sort.Strings(ips)
		out = append(out, report.Container{
			Ports:   ports,
			IPs:     ips,
			ID:      a.ID,
			Name:    name,
			Image:   a.Image,
			Project: a.Labels["com.docker.compose.project"],
			State:   a.State,
			Status:  a.Status,
			Health:  healthFromStatus(a.Status),
			Created: a.Created,
		})
	}
	return out, nil
}

// MapPath is where the helper serves the map sample (netmap), next to the
// Docker reads it passes on.
const MapPath = "/obs/map"

// Map asks the helper for a map sample.
func (c *Client) Map(ctx context.Context) (*report.Map, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://helper"+MapPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// An older helper refuses the path as not a Docker read.
		return nil, fmt.Errorf("helper: %s (update the helper for the map sample)", resp.Status)
	}
	var m report.Map
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("helper: %w", err)
	}
	return &m, nil
}

// MachinePath is where the helper serves the machine's SMBIOS UUID, which
// only root may read.
const MachinePath = "/obs/machine"

// Machine asks the helper for the machine's SMBIOS UUID; "" when the
// firmware gives none.
func (c *Client) Machine(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://helper"+MachinePath, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("helper: %s (update the helper for the machine UUID)", resp.Status)
	}
	var m struct {
		UUID string `json:"uuid"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&m); err != nil {
		return "", fmt.Errorf("helper: %w", err)
	}
	return report.MachineUUID(m.UUID), nil
}

// VMsPath is where the helper serves the host's libvirt domains.
const VMsPath = "/obs/vms"

// VMs asks the helper for the host's libvirt domains: libvirt.ErrAbsent
// where libvirt isn't running, or the helper is too old to ask.
func (c *Client) VMs(ctx context.Context) ([]libvirt.Domain, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://helper"+VMsPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, libvirt.ErrAbsent // an older helper refuses the path as not a Docker read
	}
	var v struct {
		Absent  bool             `json:"absent"`
		Error   string           `json:"error"`
		Domains []libvirt.Domain `json:"domains"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&v); err != nil {
		return nil, fmt.Errorf("helper: %w", err)
	}
	switch {
	case v.Absent:
		return nil, libvirt.ErrAbsent
	case v.Error != "":
		return nil, errors.New(v.Error)
	}
	return v.Domains, nil
}

// healthFromStatus reads the health check result out of Docker's status
// text, e.g. "Up 3 hours (healthy)".
func healthFromStatus(s string) string {
	switch {
	case strings.Contains(s, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(s, "(healthy)"):
		return "healthy"
	case strings.Contains(s, "(health: starting)"):
		return "starting"
	}
	return ""
}

// allowed are the only Docker API paths the helper passes on, after any
// /v1.NN version prefix is removed. All are reads.
var allowed = []*regexp.Regexp{
	regexp.MustCompile(`^/_ping$`),
	regexp.MustCompile(`^/version$`),
	regexp.MustCompile(`^/info$`),
	regexp.MustCompile(`^/containers/json$`),
	regexp.MustCompile(`^/containers/[A-Za-z0-9][A-Za-z0-9_.-]*/json$`),
	regexp.MustCompile(`^/images/json$`),
	regexp.MustCompile(`^/system/df$`),
	logsPath, // with a checked query: see logsQuery
}

// logsPath is one container's log. The helper only lets it through as "the
// last N lines" (logsQuery): never a follow stream, never a time range.
var logsPath = regexp.MustCompile(`^/containers/[A-Za-z0-9][A-Za-z0-9_.-]*/logs$`)

// maxLogLines is the most a log read may ask for.
const maxLogLines = 200

// logsQuery checks a log read's query and returns the only form passed on:
// stdout, stderr and timestamps on, tail=N with 1 <= N <= maxLogLines.
func logsQuery(q url.Values) (string, bool) {
	for k := range q {
		switch k {
		case "stdout", "stderr", "timestamps", "tail":
		default:
			return "", false // follow, since, until, details...
		}
	}
	n, err := strconv.Atoi(q.Get("tail"))
	if err != nil || n < 1 || n > maxLogLines {
		return "", false
	}
	return "stdout=1&stderr=1&timestamps=1&tail=" + strconv.Itoa(n), true
}

var apiVersion = regexp.MustCompile(`^/v[0-9]+\.[0-9]+(/|$)`)

// Allowed reports whether the helper may pass a request on to Docker.
func Allowed(method, p string) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	if p == "" || p != path.Clean(p) {
		return false // no "..", no doubled slashes, no trailing tricks
	}
	if m := apiVersion.FindString(p); m != "" {
		p = "/" + strings.TrimPrefix(p[len(m):], "/")
	}
	for _, re := range allowed {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// ErrNoDocker: the helper runs, but this host has no Docker. The agent
// then reports no containers at all, as it does without a helper.
var ErrNoDocker = errors.New("docker: not on this host")

// noDockerHeader marks the helper's answer when Docker's socket is missing.
const noDockerHeader = "X-Obs-Helper-No-Docker"

// Gate is the helper's HTTP handler: a reverse proxy to the Docker socket
// that refuses everything Allowed doesn't name.
func Gate(dockerSocket string) http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "docker"
			pr.Out.Host = "docker"
		},
		Transport: unixTransport(dockerSocket),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Allowed(r.Method, r.URL.Path) || r.URL.RawPath != "" {
			http.Error(w, "obs-beacon helper: not an allowed read", http.StatusForbidden)
			return
		}
		p := r.URL.Path
		if m := apiVersion.FindString(p); m != "" {
			p = "/" + strings.TrimPrefix(p[len(m):], "/")
		}
		if logsPath.MatchString(p) {
			q, ok := logsQuery(r.URL.Query())
			if !ok {
				http.Error(w, "obs-beacon helper: logs only as tail=1..200", http.StatusForbidden)
				return
			}
			r.URL.RawQuery = q
		}
		if _, err := os.Stat(dockerSocket); err != nil {
			// The helper also runs on hosts without Docker, for the map
			// sample; Docker may be installed later, so look each time.
			w.Header().Set(noDockerHeader, "1")
			http.Error(w, "obs-beacon helper: no Docker on this host", http.StatusNotFound)
			return
		}
		r.Body = http.NoBody
		rp.ServeHTTP(w, r)
	})
}

// Logs returns the last n lines of a container's output (stdout and
// stderr together, oldest first), each cut to 300 characters.
func (c *Client) Logs(ctx context.Context, id string, n int) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://docker/containers/%s/logs?stdout=1&stderr=1&timestamps=1&tail=%d", url.PathEscape(id), n), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker logs: %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, err
	}
	return tidy(splitLog(demux(raw), n)), nil
}

// tidy puts lines in time order (stdout and stderr arrive as separate
// streams) and shortens Docker's nanosecond timestamps to seconds.
func tidy(lines []string) []string {
	type line struct {
		at   time.Time
		text string
	}
	ls := make([]line, 0, len(lines))
	for _, l := range lines {
		ts, rest, _ := strings.Cut(l, " ")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			ls = append(ls, line{text: l})
			continue
		}
		ls = append(ls, line{at: t, text: t.UTC().Format("2006-01-02 15:04:05Z") + " " + rest})
	}
	sort.SliceStable(ls, func(i, j int) bool { return !ls[i].at.IsZero() && !ls[j].at.IsZero() && ls[i].at.Before(ls[j].at) })
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.text
	}
	return out
}

// demux strips Docker's 8-byte frame headers (stream, 0, 0, 0, size) from
// a container without a TTY; a TTY container's log is plain text already.
func demux(b []byte) []byte {
	if len(b) < 8 || b[0] > 2 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
		return b
	}
	var out []byte
	for len(b) >= 8 {
		size := int(binary.BigEndian.Uint32(b[4:8]))
		b = b[8:]
		if size > len(b) {
			size = len(b)
		}
		out = append(out, b[:size]...)
		b = b[size:]
	}
	return out
}

func splitLog(b []byte, n int) []string {
	lines := strings.Split(strings.TrimRight(strings.ToValidUTF8(string(b), "?"), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := lines[:0]
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if len(l) > 300 {
			l = l[:300] + "…"
		}
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// CertFilesPath is where the helper serves the certificates in the files
// the agent's globs match (docs/cert-inventory.md, step 3).
const CertFilesPath = "/obs/certfiles"

// CertFiles asks the helper for the certificates the globs' files hold:
// the certificates alone, never a key.
func (c *Client) CertFiles(ctx context.Context, globs []string) ([]report.CertFile, error) {
	q := url.Values{"glob": globs}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://helper"+CertFilesPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("helper: %s (update the helper for certificate files)", resp.Status)
	}
	var v struct {
		Files []report.CertFile `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&v); err != nil {
		return nil, fmt.Errorf("helper: %w", err)
	}
	return v.Files, nil
}

// ReloadPath is the helper's one endpoint that changes something: reload
// a unit serving an old certificate (docs/cert-rotation.md, step 2).
const ReloadPath = "/obs/reload"

// Reload asks the helper to reload a unit for a renewed certificate. The
// helper checks the request against agent.yaml's cert_reload and the
// files itself; an error says why it didn't.
func (c *Client) Reload(ctx context.Context, req certreload.Request) (certreload.Result, error) {
	b, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://helper"+ReloadPath, bytes.NewReader(b))
	if err != nil {
		return certreload.Result{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(hr)
	if err != nil {
		return certreload.Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return certreload.Result{}, fmt.Errorf("helper: %s (update the helper for reloads)", resp.Status)
	}
	var v struct {
		Result certreload.Result `json:"result"`
		Error  string            `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return certreload.Result{}, fmt.Errorf("helper: %w", err)
	}
	if v.Error != "" {
		return v.Result, errors.New(v.Error)
	}
	return v.Result, nil
}

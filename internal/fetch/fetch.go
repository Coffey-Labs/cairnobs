// Package fetch reads status pages on the agent's own host for obs-beacond:
// plain GETs, loopback or private addresses only, small answers.
package fetch

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cairnobs/cairnobs/internal/report"
	"github.com/cairnobs/cairnobs/internal/version"
)

// Spec is one page to read.
type Spec struct {
	ID  string `json:"id"`
	URL string `json:"url"`
	// Keep, when set, keeps only the lines starting with one of these
	// (a metrics page is big; obs-beacond only needs a few of its lines).
	Keep []string `json:"keep,omitempty"`
}

// maxBody is how much of an answer is kept.
const maxBody = 256 << 10

// Check refuses a URL that isn't plain http(s) to a loopback or private
// address: obs-beacond can ask an agent to read its host's status pages, not
// to fetch from the internet.
func (s Spec) Check() error {
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return fmt.Errorf("fetch %s: not a plain http(s) URL", s.ID)
	}
	ip := net.ParseIP(u.Hostname())
	if u.Hostname() == "localhost" {
		ip = net.IPv4(127, 0, 0, 1)
	}
	if ip == nil || !(ip.IsLoopback() || ip.IsPrivate()) {
		return fmt.Errorf("fetch %s: only loopback or private addresses", s.ID)
	}
	return nil
}

var client = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Get reads one page.
func Get(ctx context.Context, s Spec) report.Fetch {
	f := report.Fetch{ID: s.ID, At: time.Now().UTC()}
	if err := s.Check(); err != nil {
		f.Error = err.Error()
		return f
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL, nil)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	req.Header.Set("User-Agent", "obs-beacon-agent/"+version.Version)
	resp, err := client.Do(req)
	if err != nil {
		f.Error = err.Error()
		return f
	}
	defer resp.Body.Close()
	if len(s.Keep) == 0 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		f.Status, f.Body = resp.StatusCode, string(b)
		return f
	}
	var out strings.Builder
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 16<<20))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() && out.Len() < maxBody {
		line := sc.Text()
		for _, k := range s.Keep {
			if strings.HasPrefix(line, k) {
				out.WriteString(line + "\n")
				break
			}
		}
	}
	f.Status, f.Body = resp.StatusCode, out.String()
	return f
}

package check

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/cairnobs/cairnobs/internal/report"
)

// DefaultLists are block lists that answer ordinary resolvers. Spamhaus
// isn't here: it refuses queries from public and hosting resolvers
// (answering 127.255.255.254), so it needs its own resolver or a key.
var DefaultLists = []string{"bl.spamcop.net", "b.barracudacentral.org", "dnsbl-1.uceprotect.net", "psbl.surriel.com", "bl.mailspike.net"}

// runRBL looks the address up on every list at once. Listed anywhere
// fails the check; a list that won't answer us is noted, not counted.
func runRBL(ctx context.Context, s Spec, r *report.CheckResult) error {
	o := net.ParseIP(s.RBL).To4()
	rev := fmt.Sprintf("%d.%d.%d.%d", o[3], o[2], o[1], o[0])
	res := resolver(s)
	type answer struct {
		list   string
		listed []string
		refuse bool
		err    error
	}
	answers := make([]answer, len(s.Lists))
	var wg sync.WaitGroup
	for i, list := range s.Lists {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := answer{list: list}
			addrs, err := res.LookupHost(ctx, rev+"."+list)
			var de *net.DNSError
			switch {
			case errors.As(err, &de) && de.IsNotFound:
				// not listed
			case err != nil:
				a.err = err
			default:
				for _, ad := range addrs {
					if strings.HasPrefix(ad, "127.255.255.") {
						a.refuse = true // the list's way of saying "not from your resolver"
					} else if strings.HasPrefix(ad, "127.") {
						a.listed = append(a.listed, ad)
					}
				}
			}
			answers[i] = a
		}()
	}
	wg.Wait()
	var listed, refused, failed []string
	clean := 0
	for _, a := range answers {
		switch {
		case len(a.listed) > 0:
			sort.Strings(a.listed)
			listed = append(listed, a.list+" ("+strings.Join(a.listed, ", ")+")")
		case a.refuse:
			refused = append(refused, a.list)
		case a.err != nil:
			failed = append(failed, a.list)
		default:
			clean++
		}
	}
	parts := []string{fmt.Sprintf("clean on %d of %d", clean, len(s.Lists))}
	if len(refused) > 0 {
		parts = append(parts, "won't answer us: "+strings.Join(refused, ", "))
	}
	if len(failed) > 0 {
		parts = append(parts, "no answer: "+strings.Join(failed, ", "))
	}
	r.Answer = short(strings.Join(parts, "; "))
	switch {
	case len(listed) > 0:
		return fmt.Errorf("listed on %s", strings.Join(listed, "; "))
	case clean == 0:
		return errors.New("no block list could be asked: " + r.Answer)
	}
	return nil
}

// resolver is the host's, or the check's own server.
func resolver(s Spec) *net.Resolver {
	if s.Server == "" {
		return net.DefaultResolver
	}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, s.Server)
	}}
}

// Package flasharray reads a Pure Storage FlashArray through Purity's REST
// API 2 (docs/pure.md, step 2), as a user holding the built-in readonly
// role, with an API token: the array itself, its controllers, hardware,
// drives, volumes, ActiveCluster pods, replication connections, open
// alerts and performance. Only GET is asked, after logging in.
package flasharray

import "github.com/cairnobs/cairnobs/internal/purerest"

// Config is one array in the agent's pure: section.
type Config = purerest.Config

// Client asks one FlashArray, read only.
type Client struct{ *purerest.Client }

// New checks cfg and reads the token and the CA; it doesn't connect yet.
func New(cfg Config) (*Client, error) {
	if cfg.Type == "" {
		cfg.Type = purerest.FlashArray
	}
	c, err := purerest.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{c}, nil
}

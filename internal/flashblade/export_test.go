package flashblade

import (
	"github.com/cairnobs/cairnobs/internal/purerest"
)

// newAs is a client with cfg's type as it is: a FlashBlade set up as a
// FlashArray, for TestWrongType.
func newAs(cfg Config) (*Client, error) {
	c, err := purerest.New(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{c}, nil
}

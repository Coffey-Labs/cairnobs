// Package searchclient adapts the generated gRPC SearchServiceClient to
// the narrow querylang/executor.SearchClient interface (Search(ctx,
// query string, limit uint32) ([]string, error), satisfied structurally,
// no adapter type needed), so the query executor doesn't need to know
// anything about gRPC/protobuf directly.
package searchclient

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	searchv1 "github.com/sentry/sentry/proto/sentry/search/v1"
)

type Client struct {
	grpc searchv1.SearchServiceClient
	conn *grpc.ClientConn
}

// Dial connects to the search service. Plain TCP, no TLS: internal
// service-to-service traffic (api <-> search), same trust boundary as
// api's existing plain-TCP connection to ClickHouse -- mTLS in this
// project is specifically the agent<->ingest edge boundary, not every
// internal hop.
func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dialing search service at %s: %w", addr, err)
	}
	return &Client{grpc: searchv1.NewSearchServiceClient(conn), conn: conn}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) Search(ctx context.Context, query string, limit uint32) ([]string, error) {
	resp, err := c.grpc.Search(ctx, &searchv1.SearchRequest{Query: query, Limit: limit})
	if err != nil {
		return nil, err
	}
	return resp.GetRecordIds(), nil
}

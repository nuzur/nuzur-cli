// Package redis is a minimal stand-in for go-redis used by scanner tests.
package redis

import "context"

type Client struct{}

func (c *Client) Set(ctx context.Context, key string, value interface{}) error { return nil }
func (c *Client) Get(ctx context.Context, key string) (string, error)          { return "", nil }

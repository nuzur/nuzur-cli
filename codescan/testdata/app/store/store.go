// Package store wraps the Redis client.
package store

import (
	"context"

	"github.com/redis/go-redis/v9"
)

type Client struct{ r *redis.Client }

func New() *Client { return &Client{r: &redis.Client{}} }

func (c *Client) SetRaw(ctx context.Context, key, value string) error {
	return c.r.Set(ctx, key, value)
}

func (c *Client) Get(ctx context.Context, key string) (string, error) { return c.r.Get(ctx, key) }

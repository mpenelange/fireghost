package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// Client forwards requests to one HTTP upstream.
type Client struct {
	baseURL string
	http    *http.Client
	bearer  string
}

// NewAuthenticated creates an upstream client that sends a configured bearer token.
func NewAuthenticated(baseURL string, client *http.Client, bearer string) *Client {
	c := New(baseURL, client)
	c.bearer = bearer
	return c
}

// New creates an upstream client.
func New(baseURL string, client *http.Client) *Client {
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: client}
}

// Post forwards a POST body and returns the raw HTTP response.
func (c *Client) Post(ctx context.Context, path, contentType string, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", contentType)
	if c.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	return c.http.Do(request)
}

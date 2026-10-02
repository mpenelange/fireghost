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

// Request describes one forwarded request. PathAndQuery is appended to the
// base URL verbatim. A non-positive ContentLength leaves the length to the
// body. Host and ForwardedProto, when set, carry the client's view of the
// request so the upstream can build public URLs.
type Request struct {
	Method, PathAndQuery, ContentType string
	Body                              io.Reader
	ContentLength                     int64
	Host, ForwardedProto              string
}

// Post forwards a POST body and returns the raw HTTP response.
func (c *Client) Post(ctx context.Context, path, contentType string, body io.Reader) (*http.Response, error) {
	return c.Send(ctx, Request{Method: http.MethodPost, PathAndQuery: path, ContentType: contentType, Body: body})
}

// Send forwards a request with any method and returns the raw HTTP response.
func (c *Client) Send(ctx context.Context, r Request) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, r.Method, c.baseURL+r.PathAndQuery, r.Body)
	if err != nil {
		return nil, err
	}
	if r.ContentLength > 0 && r.Body != nil {
		request.ContentLength = r.ContentLength
	}
	if r.ContentType != "" {
		request.Header.Set("Content-Type", r.ContentType)
	}
	if r.Host != "" {
		request.Host = r.Host
	}
	if r.ForwardedProto != "" {
		request.Header.Set("X-Forwarded-Proto", r.ForwardedProto)
	}
	if c.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	return c.http.Do(request)
}

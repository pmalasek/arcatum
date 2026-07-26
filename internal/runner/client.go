package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"arcatum/pkg/proto"
)

// Client talks to arcatum-server over HTTP. mTLS lands later; for now it's plain.
type Client struct {
	base string
	hc   *http.Client
}

// NewClient returns a client for the given server base URL.
func NewClient(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: base, hc: hc}
}

// BaseURL returns the server base URL, used to derive the restic repository address.
func (c *Client) BaseURL() string { return c.base }

// Checkin asks the server for due work.
func (c *Client) Checkin(ctx context.Context, req proto.CheckinRequest) (*proto.CheckinResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/checkin", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("checkin: server returned %s", resp.Status)
	}
	var out proto.CheckinResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamUpdates sends run updates as an ndjson stream in a single request. Updates
// are read from the channel until it is closed; the HTTP request completes after.
func (c *Client) StreamUpdates(ctx context.Context, updates <-chan proto.RunUpdate) error {
	pr, pw := io.Pipe()
	go func() {
		enc := json.NewEncoder(pw)
		for u := range updates {
			if err := enc.Encode(u); err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		pw.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/v1/runs/updates", pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("updates: server returned %s", resp.Status)
	}
	return nil
}

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

// HTTPClient exposes the underlying client so renewal reuses the same mTLS connection
// settings.
func (c *Client) HTTPClient() *http.Client { return c.hc }

// DeniedError is returned when the server refuses this runner and says why, so the
// runner can tell "your certificate was revoked, ask for a new one" apart from "an
// operator rejected you".
type DeniedError struct {
	Reason  string
	Status  string
	Message string
}

func (e *DeniedError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("server refused this runner (%s): %s", e.Reason, e.Message)
	}
	return fmt.Sprintf("server refused this runner: %s", e.Message)
}

// EnrollRequired reports whether the runner should obtain a new certificate.
func (e *DeniedError) EnrollRequired() bool { return e.Reason == "enroll_required" }

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
	if resp.StatusCode == http.StatusForbidden {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
		var body struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(payload, &body)
		reason := body.Reason
		if reason == "" {
			reason = resp.Header.Get("X-Arcatum-Reason")
		}
		msg := body.Error
		if msg == "" {
			msg = string(bytes.TrimSpace(payload))
		}
		return nil, &DeniedError{Reason: reason, Status: resp.Status, Message: msg}
	}
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

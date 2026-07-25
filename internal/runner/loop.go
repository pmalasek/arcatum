package runner

import (
	"context"
	"log"
	"os"
	"time"

	"arcatum/pkg/proto"
)

// Agent ties the client and executor together into a check-in loop.
type Agent struct {
	client   *Client
	req      proto.CheckinRequest
	workBase string // base dir for per-run temp dirs
	log      *log.Logger
}

// NewAgent builds an agent for a runner identity.
func NewAgent(client *Client, req proto.CheckinRequest, workBase string, logger *log.Logger) *Agent {
	return &Agent{client: client, req: req, workBase: workBase, log: logger}
}

// Tick performs a single check-in and runs any dispatched jobs sequentially.
func (a *Agent) Tick(ctx context.Context) error {
	resp, err := a.client.Checkin(ctx, a.req)
	if err != nil {
		return err
	}
	if len(resp.Due) == 0 {
		return nil
	}
	a.log.Printf("checkin: %d job(s) due", len(resp.Due))
	for _, d := range resp.Due {
		a.runDispatch(ctx, d)
	}
	return nil
}

// Run loops Tick every interval until the context is cancelled.
func (a *Agent) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	if err := a.Tick(ctx); err != nil {
		a.log.Printf("checkin error: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.Tick(ctx); err != nil {
				a.log.Printf("checkin error: %v", err)
			}
		}
	}
}

// runDispatch executes one job while streaming its updates to the server.
func (a *Agent) runDispatch(ctx context.Context, d proto.JobDispatch) {
	if err := os.MkdirAll(a.workBase, 0o700); err != nil {
		a.log.Printf("run=%s: workdir: %v", d.RunID, err)
		return
	}
	runCtx := ctx
	var cancel context.CancelFunc
	if d.TimeoutSec > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(d.TimeoutSec)*time.Second)
		defer cancel()
	}

	updates := make(chan proto.RunUpdate, 64)
	done := make(chan error, 1)
	go func() { done <- a.client.StreamUpdates(ctx, updates) }()

	a.log.Printf("run=%s: executing script %q (%s)", d.RunID, d.Script, d.Type)
	Execute(runCtx, d, a.workBase, func(u proto.RunUpdate) { updates <- u })
	close(updates)

	if err := <-done; err != nil {
		a.log.Printf("run=%s: streaming updates: %v", d.RunID, err)
	}
}

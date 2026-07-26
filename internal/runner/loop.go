package runner

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"arcatum/pkg/crypto"
	"arcatum/pkg/proto"
)

// TLSFiles are the mTLS paths from runner.toml. The restic subprocess needs them as
// files, since it opens its own connection to the server.
type TLSFiles struct {
	CACert string
	Cert   string
	Key    string
}

// Agent ties the client and executor together into a check-in loop.
type Agent struct {
	client   *Client
	req      proto.CheckinRequest
	workBase string // base dir for per-run temp dirs
	log      *log.Logger
	verifier crypto.Verifier
	tls      TLSFiles
}

// NewAgent builds an agent for a runner identity. When verifier is non-nil every
// dispatch must carry a valid Arcatum signature or it is refused unexecuted.
func NewAgent(client *Client, req proto.CheckinRequest, workBase string, logger *log.Logger,
	verifier crypto.Verifier, tlsFiles TLSFiles) *Agent {
	return &Agent{
		client:   client,
		req:      req,
		workBase: workBase,
		log:      logger,
		verifier: verifier,
		tls:      tlsFiles,
	}
}

// verifyDispatch checks the server's signature over the job. This is the runner's own
// guarantee — independent of the transport — that the code it is about to execute was
// issued by Arcatum and not altered on the way.
func (a *Agent) verifyDispatch(d proto.JobDispatch) error {
	if a.verifier == nil {
		return nil // development mode: no signing key configured
	}
	if len(d.Signature) == 0 {
		return fmt.Errorf("dispatch is not signed")
	}
	return a.verifier.Verify(d.SigningBytes(), d.Signature)
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
	if err := a.verifyDispatch(d); err != nil {
		// Refuse to execute, and tell the server why so it shows up as a failed run
		// rather than silently never happening.
		a.log.Printf("run=%s: REFUSED, %v", d.RunID, err)
		a.reportRefusal(ctx, d.RunID, "refused: "+err.Error())
		return
	}
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

	emit := func(u proto.RunUpdate) { updates <- u }
	if d.Type == proto.TypeRestic {
		// File backups are driven by restic, not by a shipped script.
		a.log.Printf("run=%s: restic backup for instance %q", d.RunID, d.InstanceID)
		a.executeRestic(runCtx, d, emit)
	} else {
		a.log.Printf("run=%s: executing script %q (%s)", d.RunID, d.Script, d.Type)
		Execute(runCtx, d, a.workBase, emit)
	}
	close(updates)

	if err := <-done; err != nil {
		a.log.Printf("run=%s: streaming updates: %v", d.RunID, err)
	}
}

// reportRefusal records a rejected dispatch on the server as a failed run.
func (a *Agent) reportRefusal(ctx context.Context, runID, reason string) {
	updates := make(chan proto.RunUpdate, 1)
	done := make(chan error, 1)
	go func() { done <- a.client.StreamUpdates(ctx, updates) }()
	updates <- proto.RunUpdate{RunID: runID, Kind: proto.KindFinished, ExitCode: -1, Error: reason}
	close(updates)
	if err := <-done; err != nil {
		a.log.Printf("run=%s: reporting refusal: %v", runID, err)
	}
}

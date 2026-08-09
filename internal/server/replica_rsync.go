package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"arcatum/pkg/config"
)

// The transport for the off-site copy. rsync over ssh, invoked directly — no shell, so
// nothing in a path or a password can be read as a command, the same rule the restore
// path follows for restic.
//
// Arguments are built by a pure function so the flags that make this safe (never
// deleting more than max_delete, never picking up a half-written dump, never stopping to
// ask about a host key) can be asserted in tests. Nothing here mocks rsync itself: this
// repository runs the real binary or skips the test, which is how the restic and
// install.sh paths are covered too.

// rsyncTimeoutSlack is how much longer the process is allowed to live than rsync's own
// --timeout. If rsync notices the stall first it exits with a usable message; the context
// deadline is only the backstop for a process that has stopped noticing anything.
const rsyncTimeoutSlack = time.Minute

// rsyncIOTimeout is what rsync itself treats as a dead connection. Short enough that a
// WireGuard tunnel that has gone away turns into an error while an operator is still
// looking at the screen, long enough to survive a slow directory listing.
const rsyncIOTimeout = 120 * time.Second

// rshCommand is sshCommand, indirected so the end-to-end test can put a transfer through
// a local shell instead of a network. Same reason config.systemDir is a variable: the
// alternative is a test that either needs a second machine or does not run the real
// binary at all, and the real binary is the part worth testing.
var rshCommand = sshCommand

// sshCommand builds the ssh settings that make an unattended transfer either work or
// fail, never hang or prompt.
func sshCommand(r config.Replica) string {
	opts := []string{
		"ssh",
		"-p", strconv.Itoa(r.SSHPort()),
		"-o", "BatchMode=yes", // never prompt for a password: nobody is at the keyboard
		"-o", "IdentitiesOnly=yes", // use the configured key, not whatever an agent offers
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
	}
	if r.SSHKey != "" {
		opts = append(opts, "-i", r.SSHKey)
	}
	if r.KnownHosts != "" {
		opts = append(opts, "-o", "StrictHostKeyChecking=yes",
			"-o", "UserKnownHostsFile="+r.KnownHosts)
	} else {
		// Without a known_hosts file there is nothing to check against, and a transfer
		// that stops to ask "unknown host, continue?" would never finish. Say so
		// explicitly rather than letting ssh decide; the start-up log warns about it.
		opts = append(opts, "-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null", "-o", "LogLevel=ERROR")
	}
	return strings.Join(opts, " ")
}

// rsyncJob is one transfer: local files onto their place under the replica's root.
type rsyncJob struct {
	// Src is the local directory whose *contents* are copied. It is always the source —
	// replication only ever reads from backup_dir, and there is no code path that puts a
	// remote path here.
	Src string
	// Files is an explicit list of individual files to copy into Dst, used instead of
	// Src where the sources do not share a directory — the keys, which are sent from
	// where they live rather than staged next to the repositories they unlock.
	Files []string
	// Dst is relative to the replica's root, e.g. "runs/run-42".
	Dst string
	// Delete propagates removals for this job. Off for anything append-only, on for the
	// reconciling passes that carry retention through to the replica.
	Delete bool
	// Include narrows a job to part of Src, everything else being excluded. One item is
	// therefore always sent as "this subtree of runs/" rather than as the subtree
	// itself: rsync creates only the last component of a destination path, so a run
	// directory addressed directly would have nowhere to land until something else had
	// created runs/ first. Deletion stays scoped to what is included.
	Include []string
	// Excludes are rsync patterns to skip on top of the built-in ones.
	Excludes []string
	// Chmod forces permissions on the copy, e.g. "D700,F600" for the keys.
	Chmod string
}

// rsyncArgs builds the command line for one job.
func rsyncArgs(r config.Replica, job rsyncJob) []string {
	args := []string{
		"--archive",      // permissions, times, symlinks — a copy, not an approximation
		"--protect-args", // paths go to the remote shell intact, whatever is in them
		"--partial",
		// An interrupted transfer of a multi-gigabyte dump resumes instead of starting
		// over, which is the difference between a flaky link that catches up and one
		// that never finishes a single item.
		"--partial-dir=.rsync-partial",
		"--timeout=" + strconv.Itoa(int(rsyncIOTimeout/time.Second)),
	}
	// data.part is a dump still being uploaded; it is not a backup until FinishRun has
	// renamed it, and copying it would put something on the replica that never becomes
	// one. The partial dir is rsync's own bookkeeping and has no business being mirrored.
	args = append(args, "--exclude="+dataPartName, "--exclude=.rsync-partial")
	// Order is the whole of rsync's filter semantics: the first rule that matches wins.
	// Exclusions go first so nothing an include names can drag data.part along, and the
	// catch-all exclusion goes last so it only sweeps up what nothing else claimed.
	for _, ex := range job.Excludes {
		args = append(args, "--exclude="+ex)
	}
	for _, in := range job.Include {
		args = append(args, "--include="+in)
	}
	if len(job.Include) > 0 {
		args = append(args, "--exclude=*")
	}
	if job.Chmod != "" {
		args = append(args, "--chmod="+job.Chmod)
	}
	if r.BWLimit > 0 {
		args = append(args, "--bwlimit="+strconv.Itoa(r.BWLimit))
	}
	if job.Delete && r.Mirror {
		// --delete-delay removes only after the new files are across, so an interrupted
		// pass never leaves the replica with less than it started with. --max-delete is
		// the second line of defence rather than the first: rsync deletes up to the
		// ceiling and only then stops, so on its own it bounds the damage instead of
		// preventing it. What prevents it is the dry run in mirrorIsSafe, which refuses
		// the pass before a single file has gone.
		args = append(args, "--delete", "--delete-delay",
			"--max-delete="+strconv.Itoa(r.MaxDelete))
	}
	args = append(args, "--stats")
	if len(job.Files) > 0 {
		args = append(args, job.Files...)
	} else {
		// The trailing slash on the source is what makes rsync copy the contents of the
		// directory into the destination rather than nesting it one level deeper.
		args = append(args, strings.TrimSuffix(job.Src, "/")+"/")
	}
	args = append(args, r.Addr()+"/"+strings.Trim(job.Dst, "/")+"/")
	return args
}

// rsyncTransferred pulls the byte count out of rsync --stats output. It is reporting
// only: a number that cannot be parsed is not worth failing a completed transfer over.
var rsyncTransferred = regexp.MustCompile(`Total transferred file size: ([0-9,]+)`)

func parseRsyncBytes(out string) int64 {
	m := rsyncTransferred.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// rsyncExitReason translates the exit codes worth naming. rsync's numbers are otherwise
// a lookup an operator should not have to do at the moment a backup is not off-site.
func rsyncExitReason(code int) string {
	switch code {
	case 10, 12, 30, 35:
		return "connection to the replica failed"
	case 255:
		// rsync passes the remote shell's status through: ssh could not connect, could
		// not authenticate, or was refused by the replica's authorized_keys.
		return "ssh to the replica failed (unreachable, wrong key, or refused)"
	case 23, 24:
		return "some files could not be transferred"
	case 25:
		return "refused: the pass would have deleted more than max_delete files"
	case 11:
		return "file I/O error on the replica (out of space?)"
	default:
		return "rsync failed"
	}
}

// rsyncIsLinkFailure says whether an exit code means "the replica is not there" rather
// than "this item did not go through". The distinction drives the health state: one
// stuck file is not an outage, and an outage is not something to blame on a file.
func rsyncIsLinkFailure(code int) bool {
	switch code {
	case 10, 12, 30, 35, 255:
		return true
	}
	return false
}

// classifyRsyncFailure turns an exec error into something an operator can act on. Both
// the dry run and the real transfer go through it, so the two cannot drift into
// describing the same failure differently.
func classifyRsyncFailure(ctx context.Context, err error, out, what string, timeout time.Duration) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%w: %s timed out after %s", errReplicaUnreachable, what, timeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		reason := fmt.Errorf("%s (rsync exit %d): %s", rsyncExitReason(code), code, lastLine(out))
		if rsyncIsLinkFailure(code) {
			return fmt.Errorf("%w: %w", errReplicaUnreachable, reason)
		}
		return reason
	}
	return fmt.Errorf("%w: %v", errReplicaUnreachable, err)
}

// errReplicaUnreachable marks a failure as "the link is down" rather than "this item is
// bad", so the health state and the queue can tell an outage from a single stuck file.
var errReplicaUnreachable = errors.New("replica unreachable")

// errTooManyDeletions refuses a mirroring pass that would remove more than the ceiling.
var errTooManyDeletions = errors.New("too many deletions")

// countPlannedDeletions asks rsync what a mirroring pass would remove, without removing
// anything. `--itemize-changes` marks a deletion with the `*deleting` prefix.
func countPlannedDeletions(ctx context.Context, bin string, r config.Replica, job rsyncJob) (int, error) {
	args := append([]string{"--dry-run", "--itemize-changes"}, rsyncArgs(r, job)...)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(cmd.Environ(), "RSYNC_RSH="+rshCommand(r))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return 0, classifyRsyncFailure(ctx, err, out.String(), "the deletion check", 0)
	}
	n := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "*deleting") {
			n++
		}
	}
	return n, nil
}

// mirrorIsSafe refuses a pass that would take too much off the replica.
//
// This is the check that makes mirroring safe to switch on at all. Propagating deletions
// means retention here reaches the replica — and so does an accident: an unmounted
// volume, a wrong backup_dir, a restore gone sideways all make this server look like one
// whose operator deleted everything, and a faithful mirror would reproduce that. Asking
// first, and refusing before anything has gone, is the difference between a copy that
// survives a mistake here and one that repeats it.
func mirrorIsSafe(ctx context.Context, bin string, r config.Replica, job rsyncJob) error {
	if !job.Delete || !r.Mirror || r.MaxDelete <= 0 {
		return nil
	}
	n, err := countPlannedDeletions(ctx, bin, r, job)
	if err != nil {
		return err
	}
	if n > r.MaxDelete {
		return fmt.Errorf("%w: the pass would delete %d files from the replica, over the "+
			"max_delete ceiling of %d — nothing was removed. Check that backup_dir is "+
			"mounted and holds what it should; raise max_delete if the deletion is intended",
			errTooManyDeletions, n, r.MaxDelete)
	}
	return nil
}

// runRsync performs one transfer and returns the bytes it moved.
//
// The process runs in its own group and is niced: replication is background work that
// must never compete with a backup still arriving. The context deadline kills the whole
// group, so a transfer that has stopped making progress cannot outlive its timeout by
// holding a child open.
func runRsync(ctx context.Context, bin string, r config.Replica, job rsyncJob, timeout time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+rsyncTimeoutSlack)
	defer cancel()

	if err := mirrorIsSafe(ctx, bin, r, job); err != nil {
		return 0, err
	}
	args := rsyncArgs(r, job)
	name, full := niceWrap(bin, args)
	cmd := exec.CommandContext(ctx, name, full...)
	cmd.Env = append(cmd.Environ(), "RSYNC_RSH="+rshCommand(r))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second

	err := cmd.Run()
	if err == nil {
		return parseRsyncBytes(out.String()), nil
	}
	if ctx.Err() == context.DeadlineExceeded && cmd.Process != nil {
		// Whatever ignored the polite signal goes now: a stalled transfer holding a child
		// open would otherwise outlive the timeout that was meant to bound it.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return 0, classifyRsyncFailure(ctx, err, out.String(), "the transfer", timeout)
}

// probeReplica checks the replica answers at all, so an outage is visible before the
// next backup rather than only once something is waiting to be sent.
func probeReplica(ctx context.Context, r config.Replica) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rsh := strings.Fields(sshCommand(r))
	host := r.Host
	if r.User != "" {
		host = r.User + "@" + host
	}
	args := append(rsh[1:], host, "true")
	cmd := exec.CommandContext(ctx, rsh[0], args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%w: no answer within 30s", errReplicaUnreachable)
		}
		return fmt.Errorf("%w: %s", errReplicaUnreachable, lastLine(out.String()))
	}
	return nil
}

// niceWrap runs the transfer at a lower CPU and I/O priority when the tools for it are
// present. Replication reads the same disk the backups are being written to, and being
// polite about it is cheaper than discovering the contention during a restore drill.
func niceWrap(bin string, args []string) (string, []string) {
	if ionice, err := exec.LookPath("ionice"); err == nil {
		// Class 2 (best-effort), lowest priority within it.
		return ionice, append([]string{"-c2", "-n7", bin}, args...)
	}
	if nice, err := exec.LookPath("nice"); err == nil {
		return nice, append([]string{"-n", "10", bin}, args...)
	}
	return bin, args
}

// lastLine returns the final non-empty line of output, which for rsync is the one that
// says what went wrong. The rest is a transfer listing nobody needs in an error message.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return "no output"
}

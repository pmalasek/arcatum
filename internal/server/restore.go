package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Restore reads back what was backed up. It runs on the *server*, against the
// repository the server already holds, using the instance's password which the server
// can decrypt. No runner is involved: getting a file back should not depend on the
// backed-up host being reachable — quite often the reason you need a restore is that it
// is not.
//
// Everything here is admin-only, and the repository is addressed by instance, so an
// operator browses exactly the backups of one instance at a time.

// resticSnapshotIDPattern matches the short and long forms restic prints.
var resticSnapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{8}([0-9a-f]{56})?$`)

// restoreTimeout bounds a single restic invocation. Listing a large snapshot is not
// instant, but it should not hang forever either.
const restoreTimeout = 10 * time.Minute

// maxListEntries caps a directory listing so one request cannot try to render a
// snapshot with a million files.
const maxListEntries = 5000

// Snapshot is one restic snapshot as shown to an operator.
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags,omitempty"`
}

// RestoreEntry is one file or directory inside a snapshot.
type RestoreEntry struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	Type  string    `json:"type"` // "file" | "dir" | …
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// ErrNoRepository means the instance cannot be restored from — it does not exist, has
// no repository yet, or has no password. These are the operator's mistakes to fix, so
// they are reported as client errors rather than as a restic failure.
var ErrNoRepository = errors.New("no repository")

// resticEnv builds the environment and arguments for running restic against an
// instance's repository. The password is decrypted here and passed through a file so it
// never appears in the process list.
func (s *Server) resticEnv(instanceID, workDir string) (args []string, env []string, err error) {
	inst, err := s.store.Instance(instanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("instance lookup failed: %w", err)
	}
	if inst == nil {
		return nil, nil, fmt.Errorf("unknown instance %q: %w", instanceID, ErrNoRepository)
	}
	password, ok := inst.Secrets[resticPasswordSecret]
	if !ok || password == "" {
		return nil, nil, fmt.Errorf("instance %q has no %s secret, so its repository cannot be opened: %w",
			instanceID, resticPasswordSecret, ErrNoRepository)
	}
	repoDir := filepath.Join(s.store.backupDir, "restic", instanceID)
	if _, err := os.Stat(repoDir); err != nil {
		return nil, nil, fmt.Errorf("instance %q has no repository yet: %w", instanceID, ErrNoRepository)
	}
	pwFile := filepath.Join(workDir, "password")
	if err := os.WriteFile(pwFile, []byte(password), 0o600); err != nil {
		return nil, nil, fmt.Errorf("write password file: %w", err)
	}
	args = []string{"--repo", repoDir, "--password-file", pwFile}
	env = append(os.Environ(), "RESTIC_CACHE_DIR="+filepath.Join(s.store.backupDir, "restic-cache"))
	return args, env, nil
}

// resticPasswordSecret is the instance secret holding the repository password. It
// mirrors the runner-side constant; both sides must agree on the name.
const resticPasswordSecret = "restic_password"

// runRestic executes restic against an instance's repository and returns its stdout.
func (s *Server) runRestic(ctx context.Context, instanceID string, extra ...string) ([]byte, error) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		return nil, fmt.Errorf("restic is not installed on the server, which restore needs: %w", err)
	}
	workDir, err := os.MkdirTemp("", "arcatum-restore-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workDir)

	base, env, err := s.resticEnv(instanceID, workDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, append(base, extra...)...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("restic %s: %s", extra[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("restic %s: %w", extra[0], err)
	}
	return out, nil
}

// restoreError distinguishes a misconfigured instance (the operator's problem, 404) from
// restic failing (the server's problem, 502), so the UI can say something useful.
func (s *Server) restoreError(w http.ResponseWriter, instanceID string, err error) {
	if errors.Is(err, ErrNoRepository) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.log.Printf("restore %s: %v", instanceID, err)
	http.Error(w, err.Error(), http.StatusBadGateway)
}

// handleSnapshots lists an instance's snapshots, newest first.
func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if err := validateInstanceID(instanceID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, err := s.runRestic(r.Context(), instanceID, "snapshots", "--json")
	if err != nil {
		s.restoreError(w, instanceID, err)
		return
	}
	var raw []struct {
		ID       string    `json:"id"`
		ShortID  string    `json:"short_id"`
		Time     time.Time `json:"time"`
		Hostname string    `json:"hostname"`
		Paths    []string  `json:"paths"`
		Tags     []string  `json:"tags"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		s.log.Printf("snapshots %s: parse: %v", instanceID, err)
		http.Error(w, "cannot read snapshot list", http.StatusInternalServerError)
		return
	}
	snaps := make([]Snapshot, 0, len(raw))
	for i := len(raw) - 1; i >= 0; i-- { // restic prints oldest first
		r := raw[i]
		snaps = append(snaps, Snapshot{
			ID: r.ID, ShortID: r.ShortID, Time: r.Time,
			Hostname: r.Hostname, Paths: r.Paths, Tags: r.Tags,
		})
	}
	writeJSON(w, snaps)
}

// handleSnapshotLS lists the direct children of a path inside a snapshot, so the UI can
// present a browsable tree rather than one enormous flat list.
func (s *Server) handleSnapshotLS(w http.ResponseWriter, r *http.Request) {
	instanceID, snapshotID, ok := s.restoreTarget(w, r)
	if !ok {
		return
	}
	dir := cleanSnapshotPath(r.URL.Query().Get("path"))

	args := []string{"ls", "--json", snapshotID}
	if dir != "" && dir != "/" {
		args = append(args, dir)
	}
	out, err := s.runRestic(r.Context(), instanceID, args...)
	if err != nil {
		s.restoreError(w, instanceID, err)
		return
	}
	entries, truncated := parseResticLS(out, dir, maxListEntries)
	writeJSON(w, map[string]any{
		"path":      dir,
		"entries":   entries,
		"truncated": truncated,
	})
}

// parseResticLS turns restic's ndjson listing into the direct children of dir.
//
// restic lists a snapshot recursively, so the filtering to one level happens here. The
// first line is the snapshot itself and is skipped.
func parseResticLS(out []byte, dir string, max int) (entries []RestoreEntry, truncated bool) {
	prefix := dir
	if prefix == "" {
		prefix = "/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	entries = []RestoreEntry{}
	seen := map[string]bool{}

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	scanner.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024) // paths can be long
	for scanner.Scan() {
		var node struct {
			MessageType string    `json:"message_type"`
			StructType  string    `json:"struct_type"` // older restic versions
			Name        string    `json:"name"`
			Type        string    `json:"type"`
			Path        string    `json:"path"`
			Size        int64     `json:"size"`
			MTime       time.Time `json:"mtime"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &node); err != nil {
			continue
		}
		kind := node.MessageType
		if kind == "" {
			kind = node.StructType
		}
		if kind != "node" || node.Path == "" {
			continue // the snapshot header line
		}
		if !strings.HasPrefix(node.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(node.Path, prefix)
		if rest == "" {
			continue
		}
		// Only direct children: anything with a further slash lives deeper.
		if i := strings.Index(rest, "/"); i >= 0 {
			// Surface the intermediate directory even if restic listed it after its
			// contents.
			name := rest[:i]
			childPath := prefix + name
			if seen[childPath] {
				continue
			}
			seen[childPath] = true
			if len(entries) >= max {
				truncated = true
				continue
			}
			entries = append(entries, RestoreEntry{Name: name, Path: childPath, Type: "dir"})
			continue
		}
		if seen[node.Path] {
			continue
		}
		seen[node.Path] = true
		if len(entries) >= max {
			truncated = true
			continue
		}
		entries = append(entries, RestoreEntry{
			Name: node.Name, Path: node.Path, Type: node.Type,
			Size: node.Size, MTime: node.MTime,
		})
	}
	return entries, truncated
}

// handleRestoreDownload streams a file, or a directory as a tar archive, straight from
// the repository to the browser. "restic dump" writes to stdout, so nothing is staged on
// disk first.
func (s *Server) handleRestoreDownload(w http.ResponseWriter, r *http.Request) {
	instanceID, snapshotID, ok := s.restoreTarget(w, r)
	if !ok {
		return
	}
	target := cleanSnapshotPath(r.URL.Query().Get("path"))
	if target == "" || target == "/" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	asArchive := r.URL.Query().Get("archive") == "tar"

	bin, err := exec.LookPath("restic")
	if err != nil {
		http.Error(w, "restic is not installed on the server, which restore needs", http.StatusNotImplemented)
		return
	}
	workDir, err := os.MkdirTemp("", "arcatum-restore-*")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(workDir)

	base, env, err := s.resticEnv(instanceID, workDir)
	if err != nil {
		s.restoreError(w, instanceID, err)
		return
	}
	args := append(base, "dump")
	if asArchive {
		args = append(args, "--archive", "tar")
	}
	args = append(args, snapshotID, target)

	ctx, cancel := context.WithTimeout(r.Context(), restoreTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Read the first chunk *before* sending headers. A path that does not exist in the
	// snapshot makes restic fail immediately with no output, and once headers are out the
	// only way to signal that would be an empty 200 — which looks like an empty file.
	first := make([]byte, 64*1024)
	n, readErr := io.ReadFull(stdout, first)
	if n == 0 {
		waitErr := cmd.Wait()
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = "no such path in this snapshot"
		}
		if waitErr != nil {
			s.log.Printf("restore %s/%s %s: %v: %s", instanceID, snapshotID, target, waitErr, msg)
			http.Error(w, msg, http.StatusNotFound)
			return
		}
		// Genuinely empty file: fall through and send an empty body.
	}

	name := path.Base(target)
	if asArchive {
		name += ".tar"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if _, err := w.Write(first[:n]); err != nil {
		cmd.Wait()
		return
	}
	copied := int64(n)

	// From here the response has started, so a later failure can only be logged.
	if readErr == nil {
		more, copyErr := copyStream(w, stdout)
		copied += more
		if copyErr != nil {
			s.log.Printf("restore %s/%s %s: after %d bytes: %v", instanceID, snapshotID, target, copied, copyErr)
		}
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		s.log.Printf("restore %s/%s %s: truncated after %d bytes: %v: %s",
			instanceID, snapshotID, target, copied, waitErr, strings.TrimSpace(stderr.String()))
		return
	}
	s.log.Printf("restore: %s/%s %s → %d bytes", instanceID, snapshotID, target, copied)
}

// copyStream copies and flushes as it goes, so a large archive starts arriving
// immediately instead of being buffered.
func copyStream(w http.ResponseWriter, r io.Reader) (int64, error) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64*1024)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			// Anything else surfaces through cmd.Wait, which has restic's stderr.
			return total, nil
		}
	}
}

// restoreTarget validates the instance and snapshot from the URL.
func (s *Server) restoreTarget(w http.ResponseWriter, r *http.Request) (instanceID, snapshotID string, ok bool) {
	instanceID = r.PathValue("id")
	snapshotID = r.PathValue("snapshot")
	if err := validateInstanceID(instanceID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", "", false
	}
	// "latest" is restic's own alias and is convenient in the UI.
	if snapshotID != "latest" && !resticSnapshotIDPattern.MatchString(snapshotID) {
		http.Error(w, "invalid snapshot id", http.StatusBadRequest)
		return "", "", false
	}
	return instanceID, snapshotID, true
}

// cleanSnapshotPath normalises a path inside a snapshot. Snapshot paths are absolute and
// resolved by restic, not by the filesystem, but normalising keeps ".." out of the
// arguments and the results predictable.
func cleanSnapshotPath(p string) string {
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	cleaned := path.Clean(p)
	if cleaned == "." {
		return "/"
	}
	return cleaned
}

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"arcatum/pkg/crypto"
)

// This is restic's REST backend, served by Arcatum itself.
//
// File backups run restic on the backed-up host but point the repository at
// "rest:https://<server>/restic/<instance>/", so the deduplicated, encrypted pack
// files travel straight to the Arcatum server. Nothing but restic's cache stays on
// the host, which is the whole point of the design.
//
// Each instance gets its own repository under backup_dir/restic/<instance>/, and a
// runner can only reach repositories of instances targeted at it.
//
// Protocol: https://restic.readthedocs.io/en/stable/100_references.html#rest-backend

// resticTypes are the object kinds a restic repository stores.
var resticTypes = map[string]bool{
	"data":      true,
	"index":     true,
	"keys":      true,
	"locks":     true,
	"snapshots": true,
}

// resticNamePattern matches restic object names: hex-encoded IDs. Anything else is
// rejected, which keeps user-supplied path segments from escaping the repository.
var resticNamePattern = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// resticAPIv2 is the content type restic uses to request the richer listing format.
const resticAPIv2 = "application/vnd.x.restic.rest.v2"

// handleRestic dispatches every request under /restic/.
func (s *Server) handleRestic(w http.ResponseWriter, r *http.Request) {
	instanceID, rest, err := splitResticPath(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.authorizeRepo(r, instanceID); err != nil {
		s.log.Printf("restic denied: instance=%s: %v", instanceID, err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	repoDir := filepath.Join(s.store.backupDir, "restic", instanceID)

	// POST /restic/<instance>/?create=true creates the repository.
	if rest == "" {
		if r.Method == http.MethodPost && r.URL.Query().Get("create") == "true" {
			s.resticCreate(w, repoDir, instanceID)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// A trailing slash means "list this type".
	if strings.HasSuffix(rest, "/") {
		objType := strings.TrimSuffix(rest, "/")
		if !resticTypes[objType] {
			http.Error(w, "unknown object type", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.resticList(w, r, repoDir, objType)
		return
	}

	path, err := resticObjectPath(repoDir, rest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodHead, http.MethodGet:
		s.resticGet(w, r, path)
	case http.MethodPost:
		s.resticPut(w, r, path, instanceID)
	case http.MethodDelete:
		s.resticDelete(w, path)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// authorizeRepo enforces that a runner only reaches repositories belonging to its own
// instances; admins may reach any (needed for inspection and, later, restore).
func (s *Server) authorizeRepo(r *http.Request, instanceID string) error {
	inst, err := s.store.Instance(instanceID)
	if err != nil {
		return fmt.Errorf("instance lookup failed")
	}
	if inst == nil {
		return fmt.Errorf("unknown instance %q", instanceID)
	}
	if !s.requireClientCert {
		return nil // development mode: no identity available
	}
	cert := peerCert(r)
	if cert == nil {
		return errors.New("client certificate required")
	}
	switch role := crypto.CertRole(cert); role {
	case crypto.RoleAdmin:
		return nil
	case crypto.RoleRunner:
		if cert.Subject.CommonName != inst.RunnerID {
			return fmt.Errorf("runner %q may not access the repository of instance %q",
				cert.Subject.CommonName, instanceID)
		}
		return nil
	default:
		return fmt.Errorf("certificate role %q may not access repositories", role)
	}
}

// splitResticPath splits /restic/<instance>/<rest> into its parts.
func splitResticPath(urlPath string) (instanceID, rest string, err error) {
	trimmed := strings.TrimPrefix(urlPath, "/restic/")
	if trimmed == urlPath {
		return "", "", errors.New("not a restic path")
	}
	instanceID, rest, found := strings.Cut(trimmed, "/")
	if !found {
		// "/restic/<instance>" without a trailing slash.
		return instanceID, "", validateInstanceID(instanceID)
	}
	if err := validateInstanceID(instanceID); err != nil {
		return "", "", err
	}
	return instanceID, rest, nil
}

var instanceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validateInstanceID keeps the id usable as a single directory name.
func validateInstanceID(id string) error {
	if !instanceIDPattern.MatchString(id) {
		return fmt.Errorf("invalid instance id %q", id)
	}
	return nil
}

// resticObjectPath maps a repository-relative object path to a file path, refusing
// anything that is not a known type plus a hex name.
func resticObjectPath(repoDir, rest string) (string, error) {
	if rest == "config" {
		return filepath.Join(repoDir, "config"), nil
	}
	objType, name, found := strings.Cut(rest, "/")
	if !found || !resticTypes[objType] || !resticNamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid object path %q", rest)
	}
	if objType == "data" {
		// Shard pack files the way restic's local layout does, so a repository with
		// many packs does not end up as one enormous directory.
		return filepath.Join(repoDir, "data", name[:2], name), nil
	}
	return filepath.Join(repoDir, objType, name), nil
}

// resticCreate initialises the repository directory structure.
func (s *Server) resticCreate(w http.ResponseWriter, repoDir, instanceID string) {
	dirs := []string{repoDir, filepath.Join(repoDir, "data"), filepath.Join(repoDir, "index"),
		filepath.Join(repoDir, "keys"), filepath.Join(repoDir, "locks"), filepath.Join(repoDir, "snapshots")}
	for i := 0; i < 256; i++ {
		dirs = append(dirs, filepath.Join(repoDir, "data", fmt.Sprintf("%02x", i)))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			s.log.Printf("restic create %s: %v", instanceID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	s.log.Printf("restic: repository created for instance=%s", instanceID)
	w.WriteHeader(http.StatusOK)
}

// resticGet serves an object, honouring HEAD and Range requests.
func (s *Server) resticGet(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	// ServeContent handles HEAD, Range and Content-Length for us.
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// resticPut stores an object. Writes go to a temporary file and are renamed, so a
// failed upload cannot leave a truncated pack file behind.
func (s *Server) resticPut(w http.ResponseWriter, r *http.Request, path, instanceID string) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		s.log.Printf("restic put %s: %v", instanceID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// restic treats an existing object as immutable; refuse to overwrite one.
	if _, err := os.Stat(path); err == nil {
		http.Error(w, "already exists", http.StatusForbidden)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		s.log.Printf("restic put %s: %v", instanceID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	_, err = io.Copy(tmp, r.Body)
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		s.log.Printf("restic put %s: %v", instanceID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		s.log.Printf("restic put %s: rename: %v", instanceID, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := os.Chmod(path, 0o640); err != nil {
		s.log.Printf("restic put %s: chmod: %v", instanceID, err)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) resticDelete(w http.ResponseWriter, path string) {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// resticListItem is one entry of an API v2 listing.
type resticListItem struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// resticList answers a listing request in whichever API version the client asked for.
func (s *Server) resticList(w http.ResponseWriter, r *http.Request, repoDir, objType string) {
	items, err := listResticObjects(filepath.Join(repoDir, objType), objType == "data")
	if err != nil {
		s.log.Printf("restic list %s: %v", objType, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	if strings.Contains(r.Header.Get("Accept"), resticAPIv2) {
		w.Header().Set("Content-Type", resticAPIv2)
		_ = json.NewEncoder(w).Encode(items)
		return
	}
	// API v1: a plain array of names.
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.Name
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(names)
}

// listResticObjects lists a type directory, descending one level for sharded packs.
func listResticObjects(dir string, sharded bool) ([]resticListItem, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []resticListItem{}, nil
	}
	if err != nil {
		return nil, err
	}
	items := []resticListItem{}
	for _, e := range entries {
		if e.IsDir() {
			if !sharded {
				continue
			}
			sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			for _, se := range sub {
				if it, ok := resticItem(se); ok {
					items = append(items, it)
				}
			}
			continue
		}
		if it, ok := resticItem(e); ok {
			items = append(items, it)
		}
	}
	return items, nil
}

// resticItem converts a directory entry into a listing item, skipping temporary
// upload files and anything that is not a valid object name.
func resticItem(e os.DirEntry) (resticListItem, bool) {
	if !resticNamePattern.MatchString(e.Name()) {
		return resticListItem{}, false
	}
	info, err := e.Info()
	if err != nil {
		return resticListItem{}, false
	}
	return resticListItem{Name: e.Name(), Size: info.Size()}, true
}

// ResticRepoInfo summarises a repository for the API.
type ResticRepoInfo struct {
	InstanceID string    `json:"instance_id"`
	Exists     bool      `json:"exists"`
	Bytes      int64     `json:"bytes"`
	Packs      int       `json:"packs"`
	Snapshots  int       `json:"snapshots"`
	ModTime    time.Time `json:"mod_time"`
}

// resticRepoInfo measures a repository on disk. Size is computed on demand rather than
// tracked in the database, so it stays correct even after restic prunes packs.
func (s *Server) resticRepoInfo(instanceID string) (*ResticRepoInfo, error) {
	repoDir := filepath.Join(s.store.backupDir, "restic", instanceID)
	info := &ResticRepoInfo{InstanceID: instanceID}
	if _, err := os.Stat(repoDir); os.IsNotExist(err) {
		return info, nil
	} else if err != nil {
		return nil, err
	}
	info.Exists = true
	err := filepath.WalkDir(repoDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		info.Bytes += fi.Size()
		if fi.ModTime().After(info.ModTime) {
			info.ModTime = fi.ModTime()
		}
		switch {
		case strings.Contains(path, string(os.PathSeparator)+"data"+string(os.PathSeparator)):
			info.Packs++
		case strings.Contains(path, string(os.PathSeparator)+"snapshots"+string(os.PathSeparator)):
			info.Snapshots++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// handleRepoInfo reports a repository's size and snapshot count (admin only).
func (s *Server) handleRepoInfo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := validateInstanceID(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := s.resticRepoInfo(id)
	if err != nil {
		s.log.Printf("repo info %s: %v", id, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, info)
}

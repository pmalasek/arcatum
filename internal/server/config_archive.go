package server

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	"arcatum/pkg/jobspec"
	"arcatum/pkg/version"
)

// Backing up and restoring the server's own configuration as a single file.
//
// What Arcatum knows how to back up is other people's data; its own configuration —
// which instances exist, who may log in, which runners are enrolled — lived only in
// arcatum.db, and getting it onto a new machine meant copying files by hand. This is
// that, as one download and one upload:
//
//	GET  /api/v1/config/export                        → arcatum-config-<host>-<date>.zip
//	POST /api/v1/config/import                        → what an import *would* change
//	POST /api/v1/config/import?confirm=replace-all    → actually replace it
//
// Three deliberate choices shape the format:
//
//   - It is a logical export (JSON per table), not a copy of arcatum.db. The database
//     file cannot be copied consistently while the server is writing to it, it carries
//     run history and live sessions that have no business travelling, and it would tie
//     the archive to one version of the schema. JSON is inspectable and diffable, which
//     matters for a file whose whole purpose is to be restored under pressure.
//
//   - No keys. Not the CA key, not the dispatch signing key, not the secrets master key.
//     An archive holding those would be a single file that unlocks every backup
//     repository and can mint a certificate for any host. Instance secrets therefore
//     travel as ciphertext, which means an import only works where the matching master
//     key is present — checked up front rather than discovered later.
//
//   - Import replaces everything. Instances, accounts and runners are emptied and
//     refilled from the archive, so what an import leaves behind is exactly what was
//     exported, with no leftovers from whatever the server had before.
//
// Runs and their logs and payloads are not touched: history is not configuration.
// server.toml is exported for reference but never applied — a restored listen address
// or a wrong path can lock everybody out of the machine, and that is a change an
// operator should make deliberately, with a restart in hand.
//
// Both endpoints are admin-only. An export carries password verifiers, and an import
// rewrites the accounts table: either would be a privilege escalation in a viewer's
// hands.

const (
	// configArchiveFormat versions the layout below. An importer refuses what it does
	// not recognise instead of guessing.
	configArchiveFormat = 1

	configManifestName  = "manifest.json"
	configInstancesName = "instances.json"
	configUsersName     = "users.json"
	configRunnersName   = "runners.json"
	configServerTOML    = "server.toml"

	// maxConfigArchive caps an upload. Configuration is small — certificates are the
	// largest thing in it — so anything approaching this is not an archive.
	maxConfigArchive = 32 << 20
	// maxConfigEntry caps one decompressed entry, so a small upload cannot expand into
	// an enormous one.
	maxConfigEntry = 16 << 20

	// configBackupDir is where the server drops a copy of the configuration it is about
	// to replace, under backup_dir.
	configBackupDir = "config-backups"
	// configBackupsKept bounds that directory. These are small, and the useful ones are
	// the recent ones.
	configBackupsKept = 10

	// configImportConfirm is the value the confirm parameter must carry for an import to
	// actually be applied. Without it the request is a dry run, so a POST that arrives
	// by accident reports what it would do instead of doing it.
	configImportConfirm = "replace-all"
)

// ConfigManifest describes an archive and what produced it.
type ConfigManifest struct {
	// Format is configArchiveFormat.
	Format int `json:"format"`
	// Arcatum is the build that wrote the archive, for diagnosing an old file.
	Arcatum   string    `json:"arcatum"`
	CreatedAt time.Time `json:"created_at"`
	// Host is the machine the configuration came from, so a downloaded file can be told
	// apart from another server's.
	Host   string       `json:"host,omitempty"`
	Counts ConfigCounts `json:"counts"`
	// SecretKeyIDs are the master keys the exported ciphertexts were sealed with. The
	// keys themselves are not here; this is what lets an import check whether it can
	// read the secrets before it takes them.
	SecretKeyIDs []string `json:"secret_key_ids"`
	// Scripts are the script definitions the instances refer to. They live in scripts/
	// and are deployed with the binaries, not carried here — but an import can say which
	// ones are missing instead of accepting instances that can never run.
	Scripts []string `json:"scripts"`
	// Files maps each entry to the SHA-256 of its contents, so a truncated or edited
	// archive is refused rather than half-imported.
	Files map[string]string `json:"files"`
}

// ConfigCounts is what the archive holds, for a glance at the file without unpacking it.
type ConfigCounts struct {
	Instances int `json:"instances"`
	Users     int `json:"users"`
	Runners   int `json:"runners"`
}

// ConfigTableDiff summarises what an import does to one table.
type ConfigTableDiff struct {
	Added     []string `json:"added"`
	Changed   []string `json:"changed"`
	Removed   []string `json:"removed"`
	Unchanged int      `json:"unchanged"`
}

// ConfigImportPlan is what an import would do, and — once applied — what it did. The
// dry run and the real thing answer with the same shape on purpose: the UI shows the
// plan, the operator confirms, and the response to the confirmation is comparable to
// what was shown.
type ConfigImportPlan struct {
	Manifest  *ConfigManifest `json:"manifest"`
	Instances ConfigTableDiff `json:"instances"`
	Users     ConfigTableDiff `json:"users"`
	Runners   ConfigTableDiff `json:"runners"`
	// Warnings are consequences worth reading before confirming. They do not stop an
	// import; anything that should is an error instead.
	Warnings []string `json:"warnings"`
	// Applied says whether this is a dry run or a change that has happened.
	Applied bool `json:"applied"`
	// Backup is where the replaced configuration was saved, set once applied.
	Backup string `json:"backup,omitempty"`
}

// configArchive is a parsed and checksum-verified archive.
type configArchive struct {
	Manifest ConfigManifest
	Set      ConfigSet
	// ServerTOML is the exporting server's configuration file, carried for reference.
	// It is never applied — see the note at the top of this file.
	ServerTOML []byte
}

// --- export ------------------------------------------------------------------

// archiveEntry is one file inside the zip, held in memory: a configuration is small, and
// the checksums in the manifest have to be computed before the first byte is written.
type archiveEntry struct {
	name string
	data []byte
}

// buildConfigArchive writes a configuration archive. configPath may be empty, in which
// case no server.toml is included; a config file that cannot be read is skipped rather
// than failing the export, since it is the one entry nothing depends on.
func buildConfigArchive(set *ConfigSet, host, configPath string, now time.Time) ([]byte, error) {
	var entries []archiveEntry
	for _, section := range []struct {
		name  string
		value any
	}{
		{configInstancesName, set.Instances},
		{configUsersName, set.Users},
		{configRunnersName, set.Runners},
	} {
		data, err := json.MarshalIndent(section.value, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", section.name, err)
		}
		entries = append(entries, archiveEntry{section.name, append(data, '\n')})
	}
	if configPath != "" {
		if data, err := os.ReadFile(configPath); err == nil {
			entries = append(entries, archiveEntry{configServerTOML, data})
		}
	}

	manifest := ConfigManifest{
		Format:    configArchiveFormat,
		Arcatum:   version.Version,
		CreatedAt: now.UTC(),
		Host:      host,
		Counts: ConfigCounts{
			Instances: len(set.Instances),
			Users:     len(set.Users),
			Runners:   len(set.Runners),
		},
		SecretKeyIDs: configSecretKeyIDs(set),
		Scripts:      configScripts(set),
		Files:        map[string]string{},
	}
	for _, e := range entries {
		sum := sha256.Sum256(e.data)
		manifest.Files[e.name] = hex.EncodeToString(sum[:])
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// The manifest goes first so a reader — or a person with an unzip listing — sees what
	// the file is before its contents.
	all := append([]archiveEntry{{configManifestName, append(manifestJSON, '\n')}}, entries...)
	for _, e := range all {
		hdr := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: now}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(e.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// configSecretKeyIDs collects the master keys an archive's ciphertexts need. An empty
// string stands for a v1 ciphertext, which names no key and can be tried against all of
// them.
func configSecretKeyIDs(set *ConfigSet) []string {
	seen := map[string]bool{}
	for _, in := range set.Instances {
		for _, value := range in.Secrets {
			if id, sealed := crypto.SealedKeyID(value); sealed {
				seen[id] = true
			}
		}
	}
	return sortedKeys(seen)
}

// configScripts lists the script definitions the instances depend on.
func configScripts(set *ConfigSet) []string {
	seen := map[string]bool{}
	for _, in := range set.Instances {
		if in.Script != "" {
			seen[in.Script] = true
		}
	}
	return sortedKeys(seen)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- reading -----------------------------------------------------------------

// readConfigArchive parses and verifies an archive: known format, all sections present,
// every checksum matching what the manifest claims.
func readConfigArchive(data []byte) (*configArchive, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a readable zip archive: %w", err)
	}
	files := map[string][]byte{}
	for _, f := range zr.File {
		// A path in the archive would be an attempt to write outside it, and there is no
		// reason for one: every entry is a flat name.
		if strings.ContainsAny(f.Name, "/\\") {
			return nil, fmt.Errorf("unexpected path %q in archive", f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		content, err := io.ReadAll(io.LimitReader(rc, maxConfigEntry+1))
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
		if len(content) > maxConfigEntry {
			return nil, fmt.Errorf("%s is too large to be part of a configuration archive", f.Name)
		}
		files[f.Name] = content
	}

	manifestJSON, ok := files[configManifestName]
	if !ok {
		return nil, errors.New("archive has no manifest.json — is this an Arcatum configuration export?")
	}
	arc := &configArchive{}
	if err := json.Unmarshal(manifestJSON, &arc.Manifest); err != nil {
		return nil, fmt.Errorf("manifest.json: %w", err)
	}
	if arc.Manifest.Format != configArchiveFormat {
		return nil, fmt.Errorf("archive format %d, this server understands %d",
			arc.Manifest.Format, configArchiveFormat)
	}

	for name, want := range arc.Manifest.Files {
		content, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("archive is missing %s, which its manifest lists", name)
		}
		sum := sha256.Sum256(content)
		if got := hex.EncodeToString(sum[:]); got != want {
			return nil, fmt.Errorf("%s does not match its checksum — the archive is damaged or was edited", name)
		}
	}

	for _, section := range []struct {
		name  string
		value any
	}{
		{configInstancesName, &arc.Set.Instances},
		{configUsersName, &arc.Set.Users},
		{configRunnersName, &arc.Set.Runners},
	} {
		content, ok := files[section.name]
		if !ok {
			return nil, fmt.Errorf("archive is missing %s", section.name)
		}
		if err := json.Unmarshal(content, section.value); err != nil {
			return nil, fmt.Errorf("%s: %w", section.name, err)
		}
	}
	arc.ServerTOML = files[configServerTOML]
	return arc, nil
}

// --- validation ---------------------------------------------------------------

// validateConfigSet refuses an archive this server cannot honour. Every check here is a
// state the system could not recover from through the web UI: an instance nothing can
// run, a schedule the scheduler cannot parse, an account table with nobody who may
// administer it, or secrets no loaded key can decrypt.
func (s *Server) validateConfigSet(set *ConfigSet) error {
	seenInstance := map[string]bool{}
	for _, in := range set.Instances {
		if err := validateInstanceID(in.ID); err != nil {
			return err
		}
		if seenInstance[in.ID] {
			return fmt.Errorf("instance %q appears twice in the archive", in.ID)
		}
		seenInstance[in.ID] = true
		if in.RunnerID == "" {
			return fmt.Errorf("instance %q has no runner_id", in.ID)
		}
		if _, ok := s.catalog.Get(in.Script); !ok {
			return fmt.Errorf("instance %q needs script %q, which this server does not have "+
				"— deploy it into the scripts directory first", in.ID, in.Script)
		}
		// A schedule the scheduler cannot parse would be accepted into the database and
		// then stop the server from starting again (see New).
		if _, err := in.Schedule.Spec(s.sched.Location()); err != nil {
			return fmt.Errorf("instance %q: %w", in.ID, err)
		}
	}

	seenUser := map[string]bool{}
	admins := 0
	for _, u := range set.Users {
		name := NormalizeUsername(u.Username)
		if err := validUsername(name); err != nil {
			return fmt.Errorf("account %q: %w", u.Username, err)
		}
		if seenUser[name] {
			return fmt.Errorf("account %q appears twice in the archive", name)
		}
		seenUser[name] = true
		if err := validRole(u.Role); err != nil {
			return fmt.Errorf("account %q: %w", name, err)
		}
		if err := crypto.ValidPasswordHash(u.PassHash); err != nil {
			return fmt.Errorf("account %q: %w", name, err)
		}
		if u.Role == UserRoleAdmin && !u.Disabled {
			admins++
		}
	}
	if admins == 0 {
		return errors.New("the archive has no enabled administrator — importing it would " +
			"lock everybody out of the web UI")
	}

	seenRunner := map[string]bool{}
	for _, r := range set.Runners {
		if r.ID == "" {
			return errors.New("a runner in the archive has no id")
		}
		if seenRunner[r.ID] {
			return fmt.Errorf("runner %q appears twice in the archive", r.ID)
		}
		seenRunner[r.ID] = true
		switch r.Status {
		case EnrollApproved, EnrollPending, EnrollRejected:
		default:
			return fmt.Errorf("runner %q has unknown status %q", r.ID, r.Status)
		}
	}

	return s.checkConfigSecretKeys(set)
}

// checkConfigSecretKeys refuses an archive whose secrets this server cannot decrypt.
// The master key does not travel with the archive, so an import onto a machine with a
// different one would restore instances whose passwords are unreadable — and it would
// only become visible when a backup failed at night.
func (s *Server) checkConfigSecretKeys(set *ConfigSet) error {
	missing := map[string]bool{}
	sealedValues := false
	for _, in := range set.Instances {
		for _, value := range in.Secrets {
			id, sealed := crypto.SealedKeyID(value)
			if !sealed {
				continue
			}
			sealedValues = true
			if s.store.box == nil || !s.store.box.Has(id) {
				if id == "" {
					id = "(no key id, older format)"
				}
				missing[id] = true
			}
		}
	}
	if !sealedValues || len(missing) == 0 {
		return nil
	}
	if s.store.box == nil {
		return errors.New("the archive holds encrypted secrets but this server has no " +
			"secrets master key configured — set [secrets] master_key first")
	}
	return fmt.Errorf("the archive's secrets were sealed with master key(s) %s, which this "+
		"server does not have — add them to [secrets] previous_keys, or export from a "+
		"server that shares the key", strings.Join(sortedKeys(missing), ", "))
}

// --- planning -----------------------------------------------------------------

// planConfigImport compares an archive with what is here now and collects the
// consequences worth showing before anything is changed.
func (s *Server) planConfigImport(arc *configArchive) (*ConfigImportPlan, error) {
	if err := s.validateConfigSet(&arc.Set); err != nil {
		return nil, err
	}
	current, err := s.store.ConfigSet()
	if err != nil {
		return nil, err
	}
	plan := &ConfigImportPlan{
		Manifest: &arc.Manifest,
		Instances: diffConfigTable(current.Instances, arc.Set.Instances,
			func(in ConfigInstance) string { return in.ID }, fingerprintJSON[ConfigInstance]),
		Users: diffConfigTable(current.Users, arc.Set.Users,
			func(u ConfigUser) string { return NormalizeUsername(u.Username) }, fingerprintUser),
		Runners: diffConfigTable(current.Runners, arc.Set.Runners,
			func(r ConfigRunner) string { return r.ID }, fingerprintRunner),
	}
	plan.Warnings = s.configImportWarnings(current, &arc.Set, plan)
	return plan, nil
}

// configImportWarnings describes what the operator is about to live with. These are
// consequences, not faults: each one is a legitimate outcome of replacing the
// configuration, and each one has surprised somebody at least once.
func (s *Server) configImportWarnings(current, incoming *ConfigSet, plan *ConfigImportPlan) []string {
	var out []string

	// Enrollment is the one that bites hardest. A runner the archive still has approved
	// gets its access back, and one the archive predates loses it and has to enrol again.
	incomingRunners := map[string]ConfigRunner{}
	for _, r := range incoming.Runners {
		incomingRunners[r.ID] = r
	}
	for _, r := range current.Runners {
		next, kept := incomingRunners[r.ID]
		switch {
		case !kept && r.Status == EnrollApproved:
			out = append(out, fmt.Sprintf("runner %q is approved now but is not in the archive: "+
				"it loses access and has to enrol again", r.ID))
		case kept && r.Status != EnrollApproved && next.Status == EnrollApproved:
			out = append(out, fmt.Sprintf("runner %q is %s now but approved in the archive: "+
				"its old certificate is trusted again", r.ID, r.Status))
		case kept && r.Status == EnrollApproved && next.Status != EnrollApproved:
			out = append(out, fmt.Sprintf("runner %q is approved now but %s in the archive: "+
				"it stops being accepted", r.ID, next.Status))
		}
	}

	// Backup data is keyed by instance id and is not touched by an import, so an instance
	// that disappears leaves its repository and dumps behind.
	for _, id := range plan.Instances.Removed {
		if s.instanceHasStoredData(id) {
			out = append(out, fmt.Sprintf("instance %q disappears, but its backup data stays "+
				"on disk under backup_dir — nothing is deleted", id))
		}
	}

	// A script whose parameters have changed since the export accepts the instance but
	// fails when it runs, which is a bad moment to find out.
	for _, in := range incoming.Instances {
		entry, ok := s.catalog.Get(in.Script)
		if !ok {
			continue // validation already refused this
		}
		out = append(out, configParamWarnings(entry.Manifest, in)...)
	}

	// Plaintext secrets stay plaintext until a rekey, which is easy to miss on a server
	// that has a master key and expects everything to be sealed.
	if s.store.box != nil {
		for _, in := range incoming.Instances {
			for name, value := range in.Secrets {
				if _, sealed := crypto.SealedKeyID(value); !sealed && value != "" {
					out = append(out, fmt.Sprintf("secret %q of instance %q is not encrypted in "+
						"the archive; it will be stored as-is until you re-encrypt (Keys → re-encrypt)",
						name, in.ID))
				}
			}
		}
	}

	if len(plan.Users.Added)+len(plan.Users.Changed)+len(plan.Users.Removed) > 0 {
		out = append(out, "accounts change: every session ends and everyone, including you, logs in again")
	} else {
		out = append(out, "every session ends: you log in again after the import")
	}
	return out
}

// configParamWarnings reports where an instance and the script it names have drifted
// apart. Only names are compared, never values: a secret is a ciphertext here, and
// type-checking one would report nonsense.
func configParamWarnings(m *jobspec.Manifest, in ConfigInstance) []string {
	var out []string
	declared := map[string]*jobspec.Param{}
	for i := range m.Params {
		declared[m.Params[i].Name] = &m.Params[i]
	}
	for name := range in.Params {
		if _, ok := declared[name]; !ok {
			out = append(out, fmt.Sprintf("instance %q sets %q, which script %q no longer declares",
				in.ID, name, in.Script))
		}
	}
	for name := range in.Secrets {
		if _, ok := declared[name]; !ok {
			out = append(out, fmt.Sprintf("instance %q sets secret %q, which script %q no longer declares",
				in.ID, name, in.Script))
		}
	}
	for _, p := range m.Params {
		if !p.Required || strings.TrimSpace(p.Default) != "" {
			continue
		}
		values := in.Params
		if p.Secret {
			values = in.Secrets
		}
		if strings.TrimSpace(values[p.Name]) == "" {
			out = append(out, fmt.Sprintf("instance %q has no value for %q, which script %q requires",
				in.ID, p.Name, in.Script))
		}
	}
	sort.Strings(out)
	return out
}

// instanceHasStoredData reports whether an instance still has backup data under
// backup_dir — a restic repository, or dumps from its runs.
func (s *Server) instanceHasStoredData(instanceID string) bool {
	if _, err := os.Stat(filepath.Join(s.store.backupDir, "restic", instanceID)); err == nil {
		return true
	}
	stats, err := s.store.InstanceDumpStats(instanceID)
	return err == nil && stats.Count > 0
}

// diffConfigTable classifies incoming rows against what is here now. fingerprint decides
// what counts as a change: for a runner that is its identity and enrollment, not the
// timestamp of its last check-in, so a diff shows real differences rather than the
// minutes that have passed since the export.
func diffConfigTable[T any](current, incoming []T, key func(T) string, fingerprint func(T) string) ConfigTableDiff {
	have := make(map[string]T, len(current))
	for _, item := range current {
		have[key(item)] = item
	}
	diff := ConfigTableDiff{Added: []string{}, Changed: []string{}, Removed: []string{}}
	seen := map[string]bool{}
	for _, item := range incoming {
		k := key(item)
		seen[k] = true
		existing, ok := have[k]
		switch {
		case !ok:
			diff.Added = append(diff.Added, k)
		case fingerprint(existing) != fingerprint(item):
			diff.Changed = append(diff.Changed, k)
		default:
			diff.Unchanged++
		}
	}
	for _, item := range current {
		if k := key(item); !seen[k] {
			diff.Removed = append(diff.Removed, k)
		}
	}
	sort.Strings(diff.Added)
	sort.Strings(diff.Changed)
	sort.Strings(diff.Removed)
	return diff
}

// fingerprintJSON compares a row in full, for tables where every field is configuration.
func fingerprintJSON[T any](item T) string {
	data, err := json.Marshal(item)
	if err != nil {
		return fmt.Sprintf("%v", item)
	}
	return string(data)
}

// fingerprintUser ignores the bookkeeping timestamps: an account that differs only in
// when it last logged in is the same account.
func fingerprintUser(u ConfigUser) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%v", NormalizeUsername(u.Username), u.PassHash, u.Role, u.Disabled)
}

// fingerprintRunner compares identity and enrollment. Check-in details — last seen, the
// build it reported — change by themselves and are not what an import restores.
func fingerprintRunner(r ConfigRunner) string {
	return strings.Join([]string{r.ID, r.Hostname, r.OS, r.Arch, r.Status, r.CertPEM,
		r.CertFingerprint, r.CSR}, "\x00")
}

// --- applying -----------------------------------------------------------------

// applyConfigImport replaces the configuration, having first saved what it is about to
// overwrite. The saved copy is a full archive in its own right: recovering from an
// import that turned out to be the wrong file is importing the file this wrote.
func (s *Server) applyConfigImport(arc *configArchive, now time.Time) (string, error) {
	backup, err := s.saveConfigBackup(now)
	if err != nil {
		// Refusing here is deliberate. Without the safety copy an import is irreversible,
		// and "the disk is full" is exactly the moment not to find that out afterwards.
		return "", fmt.Errorf("could not save the current configuration before replacing it: %w", err)
	}
	if err := s.store.ReplaceConfig(&arc.Set); err != nil {
		return backup, err
	}
	// The scheduler holds the old set in memory; without this, removed instances would
	// keep their slot and new ones would never come due.
	instances := make([]*Instance, 0, len(arc.Set.Instances))
	for _, in := range arc.Set.Instances {
		instances = append(instances, &Instance{ID: in.ID, Schedule: in.Schedule})
	}
	if err := s.sched.Reset(instances, now); err != nil {
		// Validation already parsed every schedule, so this cannot normally happen — but
		// the database is written, and saying so is better than a silent half-applied state.
		return backup, fmt.Errorf("configuration imported, but rescheduling failed: %w", err)
	}
	return backup, nil
}

// saveConfigBackup writes the current configuration under backup_dir and returns its
// path.
func (s *Server) saveConfigBackup(now time.Time) (string, error) {
	set, err := s.store.ConfigSet()
	if err != nil {
		return "", err
	}
	data, err := buildConfigArchive(set, configHost(), s.configPath, now)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(s.store.backupDir, configBackupDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("arcatum-config-%s.zip", now.Format("20060102-150405")))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	s.pruneConfigBackups(dir)
	return path, nil
}

// pruneConfigBackups keeps the directory bounded. The names are timestamps, so sorting
// them is sorting by age.
func (s *Server) pruneConfigBackups(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".zip") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= configBackupsKept {
		return
	}
	sort.Strings(names)
	for _, name := range names[:len(names)-configBackupsKept] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			s.log.Printf("config backup prune %s: %v", name, err)
		}
	}
}

// --- HTTP ---------------------------------------------------------------------

// handleExportConfig serves the configuration as one downloadable archive.
func (s *Server) handleExportConfig(w http.ResponseWriter, r *http.Request) {
	set, err := s.store.ConfigSet()
	if err != nil {
		s.log.Printf("config export: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := time.Now()
	host := configHost()
	data, err := buildConfigArchive(set, host, s.configPath, now)
	if err != nil {
		s.log.Printf("config export: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	name := fmt.Sprintf("arcatum-config-%s-%s.zip", safeFileToken(host), now.Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	s.log.Printf("config exported by %s: %d instance(s), %d account(s), %d runner(s)",
		actor(r), len(set.Instances), len(set.Users), len(set.Runners))
	// Secrets travel exactly as they are stored. On a server with no master key that means
	// plaintext, and the file is then as sensitive as the credentials themselves.
	if s.store.box == nil {
		s.log.Printf("  WARNING: no [secrets] master_key — instance secrets are in this " +
			"archive in plaintext")
	}
	w.Write(data)
}

// handleImportConfig accepts an archive as the raw request body and reports what it
// would change. It only changes anything when the request says confirm=replace-all, so
// a POST that arrives without one is a dry run rather than a destroyed configuration.
func (s *Server) handleImportConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigArchive))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge,
			"the upload is larger than a configuration archive should ever be")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "no archive in the request body")
		return
	}
	arc, err := readConfigArchive(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plan, err := s.planConfigImport(arc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.URL.Query().Get("confirm") != configImportConfirm {
		writeJSON(w, plan)
		return
	}

	backup, err := s.applyConfigImport(arc, time.Now())
	plan.Backup = backup
	if err != nil {
		s.log.Printf("config import by %s failed: %v", actor(r), err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	plan.Applied = true
	s.log.Printf("config imported by %s from an archive of %s: instances +%d ~%d -%d, "+
		"accounts +%d ~%d -%d, runners +%d ~%d -%d; previous configuration saved to %s",
		actor(r), arc.Manifest.CreatedAt.Format(time.RFC3339),
		len(plan.Instances.Added), len(plan.Instances.Changed), len(plan.Instances.Removed),
		len(plan.Users.Added), len(plan.Users.Changed), len(plan.Users.Removed),
		len(plan.Runners.Added), len(plan.Runners.Changed), len(plan.Runners.Removed), backup)
	// Every session was just deleted, this one included; clearing the cookie means the UI
	// shows the login form instead of a string of 401s.
	s.clearSessionCookie(w)
	writeJSON(w, plan)
}

// configHost is the machine's name, recorded in an archive so a downloaded file can be
// told apart from another server's. An unknown hostname is not worth failing over.
func configHost() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}

var unsafeFileToken = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// safeFileToken makes a hostname usable inside a download filename.
func safeFileToken(s string) string {
	s = unsafeFileToken.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "arcatum"
	}
	return s
}

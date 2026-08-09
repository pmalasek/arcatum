package server

import (
	"encoding/json"
	"fmt"
	"time"
)

// Persistence for the configuration archive: reading the three tables that make up a
// server's configuration, and replacing them wholesale. The archive format, the
// validation and the handlers are in config_archive.go, the same split as
// users.go / users_store.go.
//
// What travels is deliberately narrow. Runs are history, not configuration, and stay
// where they are; sessions are dropped rather than carried, because a session belongs to
// the browser that opened it and not to the configuration it was opened against.
//
// Secrets travel exactly as they sit in the database — as ciphertext. The master key is
// not part of an archive, which is what makes the file merely sensitive rather than
// catastrophic to lose, and is also why an import first checks that the keys the
// ciphertexts name are actually loaded here (see requiredKeyIDs).

// ConfigInstance is one instance as it appears in an archive. It mirrors Instance
// field for field with one difference that matters: Secrets holds the *stored* value,
// still encrypted. Passing one of these to a code path expecting plaintext would seal a
// ciphertext a second time, hence the separate type.
type ConfigInstance struct {
	ID       string            `json:"id"`
	Script   string            `json:"script"`
	RunnerID string            `json:"runner_id"`
	Params   map[string]string `json:"params"`
	Secrets  map[string]string `json:"secrets"` // ciphertext, as stored
	Capture  string            `json:"capture"`
	Timeout  string            `json:"timeout"`
	Schedule ScheduleJSON      `json:"schedule"`
	KeepLast int               `json:"keep_last"`
	KeepDays int               `json:"keep_days"`
}

// ConfigUser is one operator account. Unlike User it carries the password verifier:
// an archive that dropped it would restore accounts nobody could log into.
type ConfigUser struct {
	Username  string    `json:"username"`
	PassHash  string    `json:"pass_hash"`
	Role      string    `json:"role"`
	Disabled  bool      `json:"disabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	LastLogin time.Time `json:"last_login,omitempty"`
}

// ConfigRunner is one registered runner, including its certificate and enrollment
// state. The certificate is public material — the runner holds the private key — so
// carrying it costs nothing and saves re-enrolling every host after a restore.
type ConfigRunner struct {
	ID              string    `json:"id"`
	Hostname        string    `json:"hostname"`
	OS              string    `json:"os"`
	Arch            string    `json:"arch"`
	Status          string    `json:"status"`
	CSR             string    `json:"csr,omitempty"`
	CertPEM         string    `json:"cert_pem,omitempty"`
	CertFingerprint string    `json:"cert_fingerprint,omitempty"`
	CertIssuer      string    `json:"cert_issuer,omitempty"`
	EnrollIP        string    `json:"enroll_ip,omitempty"`
	Version         string    `json:"version,omitempty"`
	FirstSeen       time.Time `json:"first_seen,omitempty"`
	LastSeen        time.Time `json:"last_seen,omitempty"`
	EnrolledAt      time.Time `json:"enrolled_at,omitempty"`
	ApprovedAt      time.Time `json:"approved_at,omitempty"`
	CertNotAfter    time.Time `json:"cert_not_after,omitempty"`
	RevokedAt       time.Time `json:"revoked_at,omitempty"`
	RenewedAt       time.Time `json:"renewed_at,omitempty"`
}

// ConfigSet is the whole of a server's configuration: what an export writes and what an
// import puts back.
type ConfigSet struct {
	Instances []ConfigInstance `json:"instances"`
	Users     []ConfigUser     `json:"users"`
	Runners   []ConfigRunner   `json:"runners"`
}

// ConfigSet reads the current configuration, secrets included but not decrypted.
func (s *Store) ConfigSet() (*ConfigSet, error) {
	instances, err := s.configInstances()
	if err != nil {
		return nil, err
	}
	users, err := s.configUsers()
	if err != nil {
		return nil, err
	}
	runners, err := s.configRunners()
	if err != nil {
		return nil, err
	}
	return &ConfigSet{Instances: instances, Users: users, Runners: runners}, nil
}

func (s *Store) configInstances() ([]ConfigInstance, error) {
	rows, err := s.db.Query(`SELECT ` + instanceCols + ` FROM instances ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConfigInstance{}
	for rows.Next() {
		var in ConfigInstance
		var params, secrets, sched string
		if err := rows.Scan(&in.ID, &in.Script, &in.RunnerID, &params, &secrets, &in.Capture,
			&in.Timeout, &sched, &in.KeepLast, &in.KeepDays); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(params), &in.Params); err != nil {
			return nil, fmt.Errorf("instance %q params: %w", in.ID, err)
		}
		if err := json.Unmarshal([]byte(secrets), &in.Secrets); err != nil {
			return nil, fmt.Errorf("instance %q secrets: %w", in.ID, err)
		}
		if err := json.Unmarshal([]byte(sched), &in.Schedule); err != nil {
			return nil, fmt.Errorf("instance %q schedule: %w", in.ID, err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) configUsers() ([]ConfigUser, error) {
	rows, err := s.db.Query(`SELECT username, pass_hash, role, disabled, created_at,
		updated_at, last_login FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConfigUser{}
	for rows.Next() {
		var u ConfigUser
		var disabled int
		var created, updated, login int64
		if err := rows.Scan(&u.Username, &u.PassHash, &u.Role, &disabled, &created, &updated,
			&login); err != nil {
			return nil, err
		}
		u.Disabled = disabled != 0
		u.CreatedAt, u.UpdatedAt, u.LastLogin = fromMillis(created), fromMillis(updated), fromMillis(login)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) configRunners() ([]ConfigRunner, error) {
	rows, err := s.db.Query(`SELECT id, hostname, os, arch, status, csr, cert_pem,
		cert_fingerprint, cert_issuer, enroll_ip, version, first_seen, last_seen,
		enrolled_at, approved_at, cert_not_after, revoked_at, renewed_at
		FROM runners ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConfigRunner{}
	for rows.Next() {
		var r ConfigRunner
		var first, last, enrolled, approved, notAfter, revoked, renewed int64
		if err := rows.Scan(&r.ID, &r.Hostname, &r.OS, &r.Arch, &r.Status, &r.CSR, &r.CertPEM,
			&r.CertFingerprint, &r.CertIssuer, &r.EnrollIP, &r.Version, &first, &last,
			&enrolled, &approved, &notAfter, &revoked, &renewed); err != nil {
			return nil, err
		}
		r.FirstSeen, r.LastSeen = fromMillis(first), fromMillis(last)
		r.EnrolledAt, r.ApprovedAt = fromMillis(enrolled), fromMillis(approved)
		r.CertNotAfter, r.RevokedAt, r.RenewedAt = fromMillis(notAfter), fromMillis(revoked), fromMillis(renewed)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplaceConfig swaps the configuration for the one in set: instances, accounts and
// runners are emptied and refilled, and every session is dropped.
//
// It is one transaction, so a failure part-way leaves the server exactly as it was.
// That matters more here than anywhere else in the store: a half-applied import could
// leave the system without an administrator, or with instances pointing at runners that
// were not written.
//
// Sessions go because the accounts they belong to have just been replaced. Anyone
// logged in — including whoever triggered the import — logs in again afterwards, which
// is the honest outcome: the account they were using may no longer exist.
//
// Runs are left alone. They are what happened, not what is configured, and rewriting
// history to match a restored configuration would make the two disagree with the files
// actually on disk.
func (s *Store) ReplaceConfig(set *ConfigSet) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, table := range []string{"instances", "users", "runners", "sessions"} {
		if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
			return fmt.Errorf("clear %s: %w", table, err)
		}
	}

	for _, in := range set.Instances {
		params, err := json.Marshal(orEmptyMap(in.Params))
		if err != nil {
			return err
		}
		secrets, err := json.Marshal(orEmptyMap(in.Secrets))
		if err != nil {
			return err
		}
		sched, err := json.Marshal(in.Schedule)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO instances (id, script, runner_id, params, secrets, capture, timeout,
			                       schedule, keep_last, keep_days)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.ID, in.Script, in.RunnerID, string(params), string(secrets), in.Capture,
			in.Timeout, string(sched), in.KeepLast, in.KeepDays); err != nil {
			return fmt.Errorf("insert instance %q: %w", in.ID, err)
		}
	}

	for _, u := range set.Users {
		disabled := 0
		if u.Disabled {
			disabled = 1
		}
		if _, err := tx.Exec(`
			INSERT INTO users (username, pass_hash, role, disabled, created_at, updated_at,
			                   last_login)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			u.Username, u.PassHash, u.Role, disabled, toMillis(u.CreatedAt),
			toMillis(u.UpdatedAt), toMillis(u.LastLogin)); err != nil {
			return fmt.Errorf("insert user %q: %w", u.Username, err)
		}
	}

	for _, r := range set.Runners {
		if _, err := tx.Exec(`
			INSERT INTO runners (id, hostname, os, arch, status, csr, cert_pem,
			                     cert_fingerprint, cert_issuer, enroll_ip, version,
			                     first_seen, last_seen, enrolled_at, approved_at,
			                     cert_not_after, revoked_at, renewed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, r.Hostname, r.OS, r.Arch, r.Status, r.CSR, r.CertPEM, r.CertFingerprint,
			r.CertIssuer, r.EnrollIP, r.Version, toMillis(r.FirstSeen), toMillis(r.LastSeen),
			toMillis(r.EnrolledAt), toMillis(r.ApprovedAt), toMillis(r.CertNotAfter),
			toMillis(r.RevokedAt), toMillis(r.RenewedAt)); err != nil {
			return fmt.Errorf("insert runner %q: %w", r.ID, err)
		}
	}
	return tx.Commit()
}

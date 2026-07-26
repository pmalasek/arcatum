package server

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Enrollment status of a runner.
const (
	// EnrollApproved is also the default for runners whose certificate was issued by
	// hand, so existing installations keep working after an upgrade.
	EnrollApproved = "approved"
	EnrollPending  = "pending"
	EnrollRejected = "rejected"
)

// ErrAlreadyApproved means an approved runner tried to enrol again. Replacing the
// certificate of a working runner must be a deliberate admin action, not something a
// host on the network can trigger.
var ErrAlreadyApproved = errors.New("runner is already approved")

// SubmitEnrollment records a certificate request from a host that has no certificate
// yet. The runner keeps its private key; only the CSR is sent.
//
// Re-submitting while still pending is allowed, because re-running install.sh is a
// normal thing to do. The operator sees the request's fingerprint and source address
// at approval time, which is what makes a swapped request detectable.
func (s *Store) SubmitEnrollment(runnerID, hostname, osName, arch, csrPEM, remoteIP string, now time.Time) error {
	var status string
	err := s.db.QueryRow(`SELECT status FROM runners WHERE id = ?`, runnerID).Scan(&status)
	switch {
	case err == sql.ErrNoRows:
		// First contact from this host.
	case err != nil:
		return err
	case status == EnrollApproved:
		return ErrAlreadyApproved
	}

	ms := toMillis(now)
	_, err = s.db.Exec(`
		INSERT INTO runners (id, hostname, os, arch, first_seen, last_seen,
		                     status, csr, cert_pem, cert_fingerprint, enroll_ip, enrolled_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, '', '', ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  hostname=excluded.hostname, os=excluded.os, arch=excluded.arch,
		  status=excluded.status, csr=excluded.csr, cert_pem='', cert_fingerprint='',
		  enroll_ip=excluded.enroll_ip, enrolled_at=excluded.enrolled_at`,
		runnerID, hostname, osName, arch, ms, EnrollPending, csrPEM, remoteIP, ms)
	return err
}

// Enrollment is the state a runner needs while waiting to be approved.
type Enrollment struct {
	RunnerID string `json:"runner_id"`
	Status   string `json:"status"`
	CertPEM  string `json:"cert_pem,omitempty"`
}

// Enrollment returns the current enrollment state of a runner, or nil if unknown.
func (s *Store) Enrollment(runnerID string) (*Enrollment, error) {
	var e Enrollment
	err := s.db.QueryRow(`SELECT id, status, cert_pem FROM runners WHERE id = ?`, runnerID).
		Scan(&e.RunnerID, &e.Status, &e.CertPEM)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The certificate is only meaningful once approved.
	if e.Status != EnrollApproved {
		e.CertPEM = ""
	}
	return &e, nil
}

// PendingCSR returns the certificate request awaiting approval for a runner.
func (s *Store) PendingCSR(runnerID string) (string, error) {
	var csr, status string
	err := s.db.QueryRow(`SELECT csr, status FROM runners WHERE id = ?`, runnerID).Scan(&csr, &status)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if status != EnrollPending {
		return "", fmt.Errorf("runner %q is %s, not pending", runnerID, status)
	}
	return csr, nil
}

// ApproveEnrollment stores the signed certificate and marks the runner approved, so
// its next check-in is accepted. notAfter is the new certificate's expiry, recorded so
// the UI can warn before it runs out.
func (s *Store) ApproveEnrollment(runnerID, certPEM, fingerprint string, notAfter, now time.Time) error {
	res, err := s.db.Exec(`
		UPDATE runners SET status = ?, cert_pem = ?, cert_fingerprint = ?, approved_at = ?,
		                   cert_not_after = ?, csr = ''
		WHERE id = ? AND status = ?`,
		EnrollApproved, certPEM, fingerprint, toMillis(now), toMillis(notAfter), runnerID, EnrollPending)
	if err != nil {
		return err
	}
	return requireOneRow(res, runnerID, "approve")
}

// RejectEnrollment refuses a request and discards the CSR.
func (s *Store) RejectEnrollment(runnerID string, now time.Time) error {
	res, err := s.db.Exec(`
		UPDATE runners SET status = ?, csr = '', cert_pem = '' WHERE id = ? AND status = ?`,
		EnrollRejected, runnerID, EnrollPending)
	if err != nil {
		return err
	}
	return requireOneRow(res, runnerID, "reject")
}

// RevokeEnrollment invalidates a runner's certificate and puts it back to pending, so
// its old certificate is refused everywhere from now on. The runner notices at its next
// check-in and enrols again; an operator can also hand it a certificate by hand.
//
// Unlike rejection this is not a "no" — it is "start over", which is what a suspected
// key compromise or a decommissioned host calls for.
func (s *Store) RevokeEnrollment(runnerID string, now time.Time) error {
	res, err := s.db.Exec(`
		UPDATE runners SET status = ?, cert_pem = '', cert_fingerprint = '', csr = '',
		                   cert_not_after = 0, revoked_at = ?, approved_at = 0
		WHERE id = ? AND status != ?`,
		EnrollPending, toMillis(now), runnerID, EnrollPending)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("cannot revoke runner %q: unknown, or already awaiting approval", runnerID)
	}
	return nil
}

// RevokeAll invalidates every runner's certificate at once, for when the CA itself is
// suspected. It returns how many were affected.
func (s *Store) RevokeAll(now time.Time) (int, error) {
	res, err := s.db.Exec(`
		UPDATE runners SET status = ?, cert_pem = '', cert_fingerprint = '', csr = '',
		                   cert_not_after = 0, revoked_at = ?, approved_at = 0
		WHERE status != ?`,
		EnrollPending, toMillis(now), EnrollPending)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// RenewCertificate stores a certificate issued to an already-approved runner. Renewal
// needs no operator action: the runner proved its identity with the certificate it is
// replacing.
func (s *Store) RenewCertificate(runnerID, certPEM, fingerprint string, notAfter, now time.Time) error {
	res, err := s.db.Exec(`
		UPDATE runners SET cert_pem = ?, cert_fingerprint = ?, cert_not_after = ?, renewed_at = ?
		WHERE id = ? AND status = ?`,
		certPEM, fingerprint, toMillis(notAfter), toMillis(now), runnerID, EnrollApproved)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("cannot renew runner %q: not approved", runnerID)
	}
	return nil
}

// RunnerStatus reports a runner's enrollment status; an unknown runner reports "".
func (s *Store) RunnerStatus(runnerID string) (string, error) {
	var status string
	err := s.db.QueryRow(`SELECT status FROM runners WHERE id = ?`, runnerID).Scan(&status)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return status, err
}

func requireOneRow(res sql.Result, runnerID, action string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("cannot %s runner %q: no pending request", action, runnerID)
	}
	return nil
}

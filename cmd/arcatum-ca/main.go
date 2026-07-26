// Command arcatum-ca manages Arcatum's PKI: the CA, the server certificate, runner
// and admin client certificates, and the dispatch-signing keypair.
//
// Usage:
//
//	arcatum-ca init      -dir pki [-cn "Arcatum CA"] [-days 3650]
//	arcatum-ca server    -dir pki -hosts 172.24.0.60,arcatum.xtuning.local [-days 825]
//	arcatum-ca runner    -dir pki -id web-01 [-days 825]
//	arcatum-ca admin     -dir pki -name petr [-days 365]
//	arcatum-ca signing    -dir pki [-name dispatch-signing-2]
//	arcatum-ca master-key -dir pki [-name secrets-master-2]
//	arcatum-ca bundle     -dir pki -out pki/ca-bundle.pem ca.pem ca-new.pem
//	arcatum-ca sign-csr   -dir pki -csr web-01.csr -out web-01.pem [-days 825]
//
// "init" also creates the dispatch-signing keypair and the secrets master key, so a
// fresh PKI needs one command.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"arcatum/pkg/crypto"
)

const day = 24 * time.Hour

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "server":
		err = cmdServer(args)
	case "runner":
		err = cmdRunner(args)
	case "admin":
		err = cmdAdmin(args)
	case "signing":
		err = cmdSigning(args)
	case "master-key":
		err = cmdMasterKey(args)
	case "bundle":
		err = cmdBundle(args)
	case "sign-csr":
		err = cmdSignCSR(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "arcatum-ca %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `arcatum-ca — manage Arcatum's PKI

  init        create the CA, the dispatch-signing keypair and the secrets master key
  server      issue the server certificate (needs -hosts)
  runner      issue a runner client certificate (needs -id, the runner_id)
  admin       issue an admin client certificate for API/web access
  signing     create a dispatch-signing keypair (-name for a rotation)
  master-key  create a secrets master key (-name for a rotation)
  bundle      combine CA certificates into one trust bundle (CA rotation)
  sign-csr    sign a runner CSR (enrollment building block)

Run "arcatum-ca <command> -h" for the flags of each command.
`)
}

// cmdInit creates the CA plus the dispatch-signing keypair.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", "pki", "output directory")
	cn := fs.String("cn", "Arcatum CA", "CA common name")
	name := fs.String("name", "ca", "base file name; use a new one to create a second CA for rotation")
	days := fs.Int("days", 3650, "validity in days")
	fs.Parse(args)

	caCert, caKey := filepath.Join(*dir, *name+".pem"), filepath.Join(*dir, *name+".key")
	if fileExists(caCert) {
		return fmt.Errorf("%s already exists — refusing to overwrite an existing CA", caCert)
	}
	ca, err := crypto.CreateCA(*cn, time.Duration(*days)*day)
	if err != nil {
		return err
	}
	if err := ca.Save(caCert, caKey); err != nil {
		return err
	}
	fmt.Printf("CA certificate: %s\nCA key:         %s (keep private)\n", caCert, caKey)
	if *name != "ca" {
		// A second CA is a rotation, not a fresh install: the signing and master keys
		// already exist and must not be replaced.
		fmt.Printf("\nThis is an additional CA. To roll over to it:\n"+
			"  1. arcatum-ca bundle -dir %s ca.pem %s.pem\n"+
			"  2. point [tls] ca_cert at the bundle, [bootstrap] ca_cert/ca_key at %s\n"+
			"  3. restart the server; runners adopt the bundle and renew onto the new CA\n"+
			"  4. once GET /api/v1/rotation reports safe_to_drop_old_ca, rebuild the bundle\n"+
			"     with only %s.pem\n", *dir, *name, *name, *name)
		return nil
	}
	if err := writeSigningKeys(*dir, "dispatch-signing"); err != nil {
		return err
	}
	return writeMasterKey(*dir, "secrets-master")
}

func cmdServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	dir := fs.String("dir", "pki", "PKI directory")
	hosts := fs.String("hosts", "", "comma-separated DNS names and IPs the server is reached at")
	cn := fs.String("cn", "arcatum-server", "certificate common name")
	days := fs.Int("days", 825, "validity in days")
	caName := fs.String("ca", "ca", "which authority to issue under")
	fs.Parse(args)

	if *hosts == "" {
		return fmt.Errorf("-hosts is required (e.g. -hosts 172.24.0.60,arcatum.xtuning.local)")
	}
	ca, err := loadCA(*dir, *caName)
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := ca.IssueServer(*cn, splitList(*hosts), time.Duration(*days)*day)
	if err != nil {
		return err
	}
	return writePair(filepath.Join(*dir, "server"), certPEM, keyPEM)
}

func cmdRunner(args []string) error {
	fs := flag.NewFlagSet("runner", flag.ExitOnError)
	dir := fs.String("dir", "pki", "PKI directory")
	id := fs.String("id", "", "runner id — must match the instance's runner_id (usually the hostname)")
	days := fs.Int("days", 825, "validity in days")
	caName := fs.String("ca", "ca", "which authority to issue under")
	fs.Parse(args)

	if *id == "" {
		return fmt.Errorf("-id is required (the runner_id the server will trust)")
	}
	ca, err := loadCA(*dir, *caName)
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := ca.IssueRunner(*id, time.Duration(*days)*day)
	if err != nil {
		return err
	}
	return writePair(filepath.Join(*dir, "runner-"+*id), certPEM, keyPEM)
}

func cmdAdmin(args []string) error {
	fs := flag.NewFlagSet("admin", flag.ExitOnError)
	dir := fs.String("dir", "pki", "PKI directory")
	name := fs.String("name", "", "admin name")
	days := fs.Int("days", 365, "validity in days")
	caName := fs.String("ca", "ca", "which authority to issue under")
	fs.Parse(args)

	if *name == "" {
		return fmt.Errorf("-name is required")
	}
	ca, err := loadCA(*dir, *caName)
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := ca.IssueAdmin(*name, time.Duration(*days)*day)
	if err != nil {
		return err
	}
	return writePair(filepath.Join(*dir, "admin-"+*name), certPEM, keyPEM)
}

func cmdSigning(args []string) error {
	fs := flag.NewFlagSet("signing", flag.ExitOnError)
	dir := fs.String("dir", "pki", "output directory")
	name := fs.String("name", "dispatch-signing", "base file name; use a new one to rotate")
	fs.Parse(args)
	return writeSigningKeys(*dir, *name)
}

func cmdMasterKey(args []string) error {
	fs := flag.NewFlagSet("master-key", flag.ExitOnError)
	dir := fs.String("dir", "pki", "output directory")
	name := fs.String("name", "secrets-master", "base file name; use a new one to rotate")
	fs.Parse(args)
	return writeMasterKey(*dir, *name)
}

// cmdBundle concatenates CA certificates into the trust bundle. During a CA rotation the
// bundle holds both the outgoing and the incoming authority, so runners accept either
// while they migrate.
func cmdBundle(args []string) error {
	fs := flag.NewFlagSet("bundle", flag.ExitOnError)
	dir := fs.String("dir", "pki", "PKI directory")
	out := fs.String("out", "", "output bundle path (default <dir>/ca-bundle.pem)")
	fs.Parse(args)

	inputs := fs.Args()
	if len(inputs) == 0 {
		return fmt.Errorf("list the CA certificates to combine, e.g. bundle ca.pem ca-new.pem")
	}
	target := *out
	if target == "" {
		target = filepath.Join(*dir, "ca-bundle.pem")
	}
	var combined []byte
	for _, in := range inputs {
		path := in
		if !filepath.IsAbs(path) && filepath.Dir(path) == "." {
			path = filepath.Join(*dir, in)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if !bytes.HasSuffix(data, []byte("\n")) {
			data = append(data, '\n')
		}
		combined = append(combined, data...)
	}
	if err := os.WriteFile(target, combined, 0o644); err != nil {
		return err
	}
	fmt.Printf("trust bundle: %s (%d authority file(s))\n", target, len(inputs))
	fmt.Printf("point [tls] ca_cert at it; keep [bootstrap] ca_cert on the authority that signs new certificates\n")
	return nil
}

func cmdSignCSR(args []string) error {
	fs := flag.NewFlagSet("sign-csr", flag.ExitOnError)
	dir := fs.String("dir", "pki", "PKI directory")
	csrPath := fs.String("csr", "", "path to the CSR in PEM form")
	out := fs.String("out", "", "output certificate path")
	days := fs.Int("days", 825, "validity in days")
	caName := fs.String("ca", "ca", "which authority to issue under")
	fs.Parse(args)

	if *csrPath == "" || *out == "" {
		return fmt.Errorf("-csr and -out are required")
	}
	ca, err := loadCA(*dir, *caName)
	if err != nil {
		return err
	}
	csrPEM, err := os.ReadFile(*csrPath)
	if err != nil {
		return err
	}
	certPEM, err := ca.SignCSR(csrPEM, time.Duration(*days)*day)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, certPEM, 0o644); err != nil {
		return err
	}
	fmt.Printf("certificate: %s\n", *out)
	return nil
}

// --- helpers ----------------------------------------------------------------

// loadCA opens one of possibly several authorities in the directory. During a CA
// rotation new certificates must be issued under the incoming one.
func loadCA(dir, name string) (*crypto.CA, error) {
	if name == "" {
		name = "ca"
	}
	return crypto.LoadCA(filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key"))
}

// writeSigningKeys creates the dispatch-signing keypair unless it already exists.
func writeSigningKeys(dir, name string) error {
	privPath := filepath.Join(dir, name+".key")
	pubPath := filepath.Join(dir, name+".pub")
	if fileExists(privPath) {
		return fmt.Errorf("%s already exists — refusing to overwrite (runners trust the matching public key)", privPath)
	}
	privPEM, pubPEM, err := crypto.GenerateSigningKey()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		return err
	}
	fmt.Printf("signing key:    %s (server, keep private)\nsigning pubkey: %s (distribute to runners)\n", privPath, pubPath)
	return nil
}

// writeMasterKey creates the secrets master key unless it already exists. Losing this
// key makes every stored secret unreadable, so overwriting is never implicit.
func writeMasterKey(dir, name string) error {
	path := filepath.Join(dir, name+".key")
	if fileExists(path) {
		return fmt.Errorf("%s already exists — refusing to overwrite (stored secrets would become unreadable)", path)
	}
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return err
	}
	fmt.Printf("secrets master key: %s (server only — back it up, losing it loses the secrets)\n", path)
	return nil
}

// writePair writes <base>.pem and <base>.key.
func writePair(base string, certPEM, keyPEM []byte) error {
	certPath, keyPath := base+".pem", base+".key"
	if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	fmt.Printf("certificate: %s\nprivate key: %s\n", certPath, keyPath)
	return nil
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

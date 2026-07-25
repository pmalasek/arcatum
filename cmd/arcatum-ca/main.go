// Command arcatum-ca manages Arcatum's PKI: the CA, the server certificate, runner
// and admin client certificates, and the dispatch-signing keypair.
//
// Usage:
//
//	arcatum-ca init      -dir pki [-cn "Arcatum CA"] [-days 3650]
//	arcatum-ca server    -dir pki -hosts 172.24.0.60,arcatum.xtuning.local [-days 825]
//	arcatum-ca runner    -dir pki -id web-01 [-days 825]
//	arcatum-ca admin     -dir pki -name petr [-days 365]
//	arcatum-ca signing   -dir pki
//	arcatum-ca sign-csr  -dir pki -csr web-01.csr -out web-01.pem [-days 825]
//
// "init" also creates the dispatch-signing keypair, so a fresh PKI needs one command.
package main

import (
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

  init      create the CA and the dispatch-signing keypair
  server    issue the server certificate (needs -hosts)
  runner    issue a runner client certificate (needs -id, the runner_id)
  admin     issue an admin client certificate for API/web access
  signing   (re)create only the dispatch-signing keypair
  sign-csr  sign a runner CSR (enrollment building block)

Run "arcatum-ca <command> -h" for the flags of each command.
`)
}

// cmdInit creates the CA plus the dispatch-signing keypair.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dir := fs.String("dir", "pki", "output directory")
	cn := fs.String("cn", "Arcatum CA", "CA common name")
	days := fs.Int("days", 3650, "validity in days")
	fs.Parse(args)

	caCert, caKey := filepath.Join(*dir, "ca.pem"), filepath.Join(*dir, "ca.key")
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
	return writeSigningKeys(*dir)
}

func cmdServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	dir := fs.String("dir", "pki", "PKI directory")
	hosts := fs.String("hosts", "", "comma-separated DNS names and IPs the server is reached at")
	cn := fs.String("cn", "arcatum-server", "certificate common name")
	days := fs.Int("days", 825, "validity in days")
	fs.Parse(args)

	if *hosts == "" {
		return fmt.Errorf("-hosts is required (e.g. -hosts 172.24.0.60,arcatum.xtuning.local)")
	}
	ca, err := loadCA(*dir)
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
	fs.Parse(args)

	if *id == "" {
		return fmt.Errorf("-id is required (the runner_id the server will trust)")
	}
	ca, err := loadCA(*dir)
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
	fs.Parse(args)

	if *name == "" {
		return fmt.Errorf("-name is required")
	}
	ca, err := loadCA(*dir)
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
	fs.Parse(args)
	return writeSigningKeys(*dir)
}

func cmdSignCSR(args []string) error {
	fs := flag.NewFlagSet("sign-csr", flag.ExitOnError)
	dir := fs.String("dir", "pki", "PKI directory")
	csrPath := fs.String("csr", "", "path to the CSR in PEM form")
	out := fs.String("out", "", "output certificate path")
	days := fs.Int("days", 825, "validity in days")
	fs.Parse(args)

	if *csrPath == "" || *out == "" {
		return fmt.Errorf("-csr and -out are required")
	}
	ca, err := loadCA(*dir)
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

func loadCA(dir string) (*crypto.CA, error) {
	return crypto.LoadCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"))
}

// writeSigningKeys creates the dispatch-signing keypair unless it already exists.
func writeSigningKeys(dir string) error {
	privPath := filepath.Join(dir, "dispatch-signing.key")
	pubPath := filepath.Join(dir, "dispatch-signing.pub")
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

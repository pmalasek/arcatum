package server

import (
	"embed"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// The bootstrap listener exists because of a chicken-and-egg problem: a freshly
// installed host has no client certificate, and the main listener requires one — such a
// client is rejected during the TLS handshake. So the few things needed to get started
// are served over plain HTTP from a separate port:
//
//	/arcatum_runner/install.sh              the installer (generated per request)
//	/arcatum_runner/arcatum-runner-os-arch  the runner binary
//	/arcatum_runner/ca.pem                  the CA certificate
//	/arcatum_runner/dispatch-signing.pub    the job-signing public key
//	/api/v1/enroll, /api/v1/enroll/{id}     certificate request and pickup
//
// None of it is secret: the CA certificate and the signing public key are public by
// design, and a CSR carries only a public key. What protects the system is that an
// operator must approve a request before the runner is trusted with any work.

//go:embed install.sh.tmpl
var installTemplateFS embed.FS

var installTemplate = template.Must(template.ParseFS(installTemplateFS, "install.sh.tmpl"))

// BootstrapConfig describes what the bootstrap listener serves.
type BootstrapConfig struct {
	// DistDir holds the runner binaries named arcatum-runner-<os>-<arch>.
	DistDir string
	// CACert is the path to the CA certificate, which the runner needs to verify the
	// server.
	CACert string
	// SigningPubPEM is the job-signing public key. It comes from the loaded signing key
	// rather than a configured path, so it cannot drift from the key actually used to
	// sign jobs.
	SigningPubPEM []byte
	// APIURL is the mTLS address runners check in to, e.g. https://172.24.0.60:8443.
	// Written into the generated runner.toml.
	APIURL string
}

// BootstrapHandler returns the handler for the plain-HTTP bootstrap listener.
func (s *Server) BootstrapHandler(cfg BootstrapConfig) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /arcatum_runner/install.sh", func(w http.ResponseWriter, r *http.Request) {
		s.serveInstallScript(w, r, cfg)
	})
	mux.HandleFunc("GET /arcatum_runner/ca.pem", func(w http.ResponseWriter, r *http.Request) {
		s.serveBootstrapFile(w, r, cfg.CACert, "application/x-pem-file")
	})
	mux.HandleFunc("GET /arcatum_runner/dispatch-signing.pub", func(w http.ResponseWriter, r *http.Request) {
		if len(cfg.SigningPubPEM) == 0 {
			http.Error(w, "not configured", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(cfg.SigningPubPEM)
	})
	mux.HandleFunc("GET /arcatum_runner/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.serveRunnerBinary(w, r, cfg.DistDir, r.PathValue("name"))
	})

	// Enrollment: reachable without a certificate, by necessity.
	mux.HandleFunc("POST /api/v1/enroll", s.handleEnroll)
	mux.HandleFunc("GET /api/v1/enroll/{id}", s.handleEnrollStatus)

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "arcatum-server bootstrap endpoint\n\nInstall a runner with:\n\n"+
			"    curl -LsSf %s/arcatum_runner/install.sh | sudo sh\n\n"+
			"The web UI is at %s/\n", bootstrapURL(r), cfg.APIURL)
	})
	return mux
}

// serveInstallScript renders the installer with the address this request came in on, so
// the runner is configured to talk back to wherever it just downloaded from.
func (s *Server) serveInstallScript(w http.ResponseWriter, r *http.Request, cfg BootstrapConfig) {
	data := struct {
		BootstrapURL string
		APIURL       string
	}{
		BootstrapURL: bootstrapURL(r),
		APIURL:       cfg.APIURL,
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	if err := installTemplate.Execute(w, data); err != nil {
		s.log.Printf("install.sh: %v", err)
	}
}

// InstallInfo tells the web UI how a runner is installed against this server, so the
// command can be read off the Runners tab instead of the documentation.
type InstallInfo struct {
	// Enabled is false when no bootstrap listener is configured, in which case there is
	// no installer to point at.
	Enabled bool   `json:"enabled"`
	URL     string `json:"url,omitempty"`
	Command string `json:"command,omitempty"`
}

// handleInstallInfo builds the one-liner that installs a runner.
//
// The host is taken from the request rather than from the configuration on purpose: the
// operator reached the web UI at an address that works from where they are, and the
// bootstrap listener is the same host on a different port. A configured "0.0.0.0:80"
// pasted into a shell would not resolve anywhere.
func (s *Server) handleInstallInfo(w http.ResponseWriter, r *http.Request) {
	if s.bootstrapListen == "" {
		writeJSON(w, InstallInfo{})
		return
	}
	url := installURL(r.Host, s.bootstrapListen)
	writeJSON(w, InstallInfo{
		Enabled: true,
		URL:     url,
		Command: "curl -LsSf " + url + " | sudo sh",
	})
}

// installURL points at install.sh on the bootstrap listener: the host the browser is
// talking to, on the bootstrap port. Plain HTTP, because that listener is plain HTTP by
// design — a host with no certificate yet cannot do anything else.
func installURL(requestHost, bootstrapListen string) string {
	host := requestHost
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		host = "localhost"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]" // IPv6 literal
	}
	if _, port, err := net.SplitHostPort(bootstrapListen); err == nil && port != "" && port != "80" {
		host += ":" + port
	}
	return "http://" + host + "/arcatum_runner/install.sh"
}

// bootstrapURL reconstructs the URL the client used, which is how install.sh learns the
// server's address without it being configured twice.
func bootstrapURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host
}

func (s *Server) serveBootstrapFile(w http.ResponseWriter, r *http.Request, path, contentType string) {
	if path == "" {
		http.Error(w, "not configured", http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		s.log.Printf("bootstrap: read %s: %v", path, err)
		http.Error(w, "not available", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Write(data)
}

// runnerBinaryPattern is what install.sh asks for: arcatum-runner-<os>-<arch>.
// Restricting the name keeps the request from reaching outside DistDir.
func validRunnerBinaryName(name string) bool {
	if !strings.HasPrefix(name, "arcatum-runner-") {
		return false
	}
	rest := strings.TrimPrefix(name, "arcatum-runner-")
	parts := strings.Split(rest, "-")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || !isAlnum(p) {
			return false
		}
	}
	return true
}

func isAlnum(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func (s *Server) serveRunnerBinary(w http.ResponseWriter, r *http.Request, distDir, name string) {
	if distDir == "" {
		http.Error(w, "no dist directory configured", http.StatusNotFound)
		return
	}
	if !validRunnerBinaryName(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	path := filepath.Join(distDir, name)
	f, err := os.Open(path)
	if err != nil {
		s.log.Printf("bootstrap: %s not available (%v)", name, err)
		http.Error(w, "binary not available for this platform", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

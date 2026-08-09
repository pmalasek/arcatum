package server

import (
	"strings"
	"testing"

	"arcatum/pkg/config"
)

func testReplicaConfig() config.Replica {
	return config.Replica{
		Enabled: true,
		Host:    "172.26.0.2",
		User:    "arcatum",
		Path:    "/data",
		SSHKey:  "/opt/arcatum/pki/replica-ssh.key",
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestRsyncArgsDestination(t *testing.T) {
	r := testReplicaConfig()
	args := rsyncArgs(r, rsyncJob{Src: "/central_backup/arcatum/runs/run-42", Dst: "runs/run-42"})

	src, dst := args[len(args)-2], args[len(args)-1]
	// The trailing slash on the source is what stops rsync nesting the directory one
	// level deeper on every pass.
	if src != "/central_backup/arcatum/runs/run-42/" {
		t.Fatalf("source = %q, want a trailing slash", src)
	}
	if dst != "arcatum@172.26.0.2:/data/runs/run-42/" {
		t.Fatalf("destination = %q", dst)
	}
	// The local path is always the source. Replication reads backup_dir and writes to the
	// replica; a reversed pair would mean the far side could overwrite the backups here.
	if strings.Contains(src, "@") || strings.Contains(src, ":") {
		t.Fatalf("source %q looks remote — replication must only ever read locally", src)
	}
}

func TestRsyncArgsNeverCopiesAnUnfinishedDump(t *testing.T) {
	args := rsyncArgs(testReplicaConfig(), rsyncJob{Src: "/b/runs", Dst: "runs"})
	if !hasArg(args, "--exclude="+dataPartName) {
		t.Fatalf("data.part must be excluded — it is not a backup until FinishRun renames it: %v", args)
	}
	if !hasArg(args, "--exclude=.rsync-partial") {
		t.Fatalf("rsync's own partial directory must not be mirrored: %v", args)
	}
	if !hasArg(args, "--partial-dir=.rsync-partial") {
		t.Fatalf("an interrupted transfer must be able to resume: %v", args)
	}
}

func TestRsyncArgsMirrorNeedsMaxDelete(t *testing.T) {
	r := testReplicaConfig()

	// Not mirroring: no deletion flags at all, whatever the job asks for.
	args := rsyncArgs(r, rsyncJob{Src: "/b/runs", Dst: "runs", Delete: true})
	if hasArg(args, "--delete") {
		t.Fatalf("mirror is off, --delete must not appear: %v", args)
	}

	r.Mirror, r.MaxDelete = true, 100
	args = rsyncArgs(r, rsyncJob{Src: "/b/runs", Dst: "runs", Delete: true})
	if !hasArg(args, "--delete") || !hasArg(args, "--delete-delay") {
		t.Fatalf("mirroring pass must delete, and only after the new files are across: %v", args)
	}
	// The guard between an unmounted backup_dir and an emptied replica.
	if !hasArg(args, "--max-delete=100") {
		t.Fatalf("mirroring pass must carry the max-delete ceiling: %v", args)
	}

	// A job that is not a reconciling pass never deletes, even while mirroring is on.
	args = rsyncArgs(r, rsyncJob{Src: "/b/restic/x", Dst: "restic/x"})
	if hasArg(args, "--delete") {
		t.Fatalf("append-only job must not delete: %v", args)
	}
}

func TestRsyncArgsBandwidthAndChmod(t *testing.T) {
	r := testReplicaConfig()
	if args := rsyncArgs(r, rsyncJob{Src: "/b/runs", Dst: "runs"}); hasArg(args, "--bwlimit=0") {
		t.Fatalf("an unset bwlimit must not be passed at all: %v", args)
	}
	r.BWLimit = 2048
	args := rsyncArgs(r, rsyncJob{Src: "/b/runs", Dst: "runs", Chmod: "D700,F600"})
	if !hasArg(args, "--bwlimit=2048") {
		t.Fatalf("bwlimit missing: %v", args)
	}
	if !hasArg(args, "--chmod=D700,F600") {
		t.Fatalf("chmod missing: %v", args)
	}
}

func TestRsyncArgsFileList(t *testing.T) {
	args := rsyncArgs(testReplicaConfig(), rsyncJob{
		Files: []string{"/pki/ca.pem", "/pki/secrets-master.key"},
		Dst:   "keys",
		Chmod: "D700,F600",
	})
	tail := args[len(args)-3:]
	if tail[0] != "/pki/ca.pem" || tail[1] != "/pki/secrets-master.key" {
		t.Fatalf("file sources not passed verbatim: %v", args)
	}
	if tail[2] != "arcatum@172.26.0.2:/data/keys/" {
		t.Fatalf("destination = %q", tail[2])
	}
	// Files given individually must not get a trailing slash appended to them.
	for _, a := range args {
		if strings.HasSuffix(a, ".pem/") || strings.HasSuffix(a, ".key/") {
			t.Fatalf("file source turned into a directory: %v", args)
		}
	}
}

func TestRsyncArgsExcludesAreOrderedForRestic(t *testing.T) {
	jobs := resticRepoJobs("/b/restic", "restic", "files-web01")
	if len(jobs) != 2 {
		t.Fatalf("want two passes, got %d", len(jobs))
	}
	// Packs first. A snapshot that arrives before the data it names leaves the replica
	// holding a repository restic will open and then fail to restore from — which looks
	// like a backup and is not one.
	first := rsyncArgs(testReplicaConfig(), jobs[0])
	if !hasArg(first, "--exclude=/files-web01/index/") || !hasArg(first, "--exclude=/files-web01/snapshots/") {
		t.Fatalf("first pass must leave the index and snapshots behind: %v", first)
	}
	if jobs[0].Delete {
		t.Fatalf("the partial first pass must never delete")
	}
	second := rsyncArgs(testReplicaConfig(), jobs[1])
	if hasArg(second, "--exclude=/files-web01/index/") {
		t.Fatalf("second pass must carry everything: %v", second)
	}
	if !jobs[1].Delete {
		t.Fatalf("the completing pass is what reconciles removals")
	}
}

func TestSSHCommandNeverPrompts(t *testing.T) {
	r := testReplicaConfig()
	r.KnownHosts = "/opt/arcatum/pki/replica-known_hosts"
	cmd := sshCommand(r)
	for _, want := range []string{
		"-o BatchMode=yes",      // a transfer that asks for a password never completes
		"-o IdentitiesOnly=yes", // use the configured key, not an agent's
		"-o ConnectTimeout=10",
		"-o ServerAliveInterval=15",
		"-o StrictHostKeyChecking=yes",
		"-o UserKnownHostsFile=/opt/arcatum/pki/replica-known_hosts",
		"-i /opt/arcatum/pki/replica-ssh.key",
		"-p 22",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("ssh command %q is missing %q", cmd, want)
		}
	}

	// Without a known_hosts file there is nothing to verify against; the choice is made
	// explicitly rather than left to ssh, which would stop and ask.
	r.KnownHosts = ""
	cmd = sshCommand(r)
	if !strings.Contains(cmd, "-o StrictHostKeyChecking=no") {
		t.Fatalf("no known_hosts: host key checking must be switched off explicitly, got %q", cmd)
	}
	if strings.Contains(cmd, "StrictHostKeyChecking=yes") {
		t.Fatalf("cannot check a host key with no known_hosts file: %q", cmd)
	}

	r.Port = 2222
	if !strings.Contains(sshCommand(r), "-p 2222") {
		t.Fatalf("configured port ignored")
	}
}

func TestParseRsyncBytes(t *testing.T) {
	out := "Number of files: 3\nTotal transferred file size: 1,234,567 bytes\n"
	if got := parseRsyncBytes(out); got != 1234567 {
		t.Fatalf("parseRsyncBytes = %d, want 1234567", got)
	}
	// Reporting only: an unparseable figure must not fail a transfer that succeeded.
	if got := parseRsyncBytes("nothing useful here"); got != 0 {
		t.Fatalf("parseRsyncBytes on unknown output = %d, want 0", got)
	}
}

func TestRsyncExitReasonNamesTheMaxDeleteGuard(t *testing.T) {
	if got := rsyncExitReason(25); !strings.Contains(got, "max_delete") {
		t.Fatalf("exit 25 should name the ceiling that stopped it, got %q", got)
	}
	if got := rsyncExitReason(30); !strings.Contains(got, "connection") {
		t.Fatalf("exit 30 should read as a link failure, got %q", got)
	}
	if got := rsyncExitReason(1); got == "" {
		t.Fatal("unknown exit codes still need a message")
	}
}

func TestReplicaKeyFilesComeFromTheConfiguration(t *testing.T) {
	cfg := &config.Config{}
	cfg.TLS = config.TLS{CACert: "/pki/ca.pem", Cert: "/pki/server.pem", Key: "/pki/server.key"}
	cfg.Signing = config.Signing{Key: "/pki/sign.key", PreviousKeys: []string{"/pki/sign-old.key"}}
	cfg.Secrets = config.Secrets{MasterKey: "/pki/master.key", PreviousKeys: []string{""}}
	// The trust bundle and the signing CA are usually the same file; sending it twice
	// would be pointless work on a link that is the scarce resource here.
	cfg.Bootstrap = config.Bootstrap{CAKey: "/pki/ca.key", CACert: "/pki/ca.pem"}

	got := ReplicaKeyFiles(cfg)
	want := []string{"/pki/ca.key", "/pki/ca.pem", "/pki/master.key", "/pki/server.key",
		"/pki/server.pem", "/pki/sign-old.key", "/pki/sign.key"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ReplicaKeyFiles =\n %v\nwant\n %v", got, want)
	}
}

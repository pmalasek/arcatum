# backup_central task runner

set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

go := env_var_or_default("GO", "go")
gofmt := env_var_or_default("GOFMT", go + "fmt")
version := env_var_or_default("V", "$(date +%Y.%m.%d)")
server_cfg := env_var_or_default("SERVER_CONFIG", "local/server.toml")
instances := env_var_or_default("INSTANCES", "local/instances.json")
server_url := env_var_or_default("SERVER_URL", "http://127.0.0.1:8443")
listen := env_var_or_default("LISTEN", "127.0.0.1:8443")
web_listen := env_var_or_default("WEB_LISTEN", "127.0.0.1:8080")

# Show available recipes.
default:
	just --list

# Build all Go packages.
build:
	{{go}} build ./...

# Build all commands to ./bin.
build-all:
	mkdir -p bin
	{{go}} build -o bin/arcatum-server ./cmd/server
	{{go}} build -o bin/arcatum-runner ./cmd/runner
	{{go}} build -o bin/arcatum-ca ./cmd/arcatum-ca

# Build version-stamped release binaries to ./bin.
release:
	mkdir -p bin
	{{go}} build -ldflags "-X arcatum/pkg/version.Version={{version}}" -o bin/arcatum-server ./cmd/server
	{{go}} build -ldflags "-X arcatum/pkg/version.Version={{version}}" -o bin/arcatum-runner ./cmd/runner
	{{go}} build -ldflags "-X arcatum/pkg/version.Version={{version}}" -o bin/arcatum-ca ./cmd/arcatum-ca

# Build runner binaries for bootstrap dist directory.
dist-runner dist_dir="local/dist":
	mkdir -p "{{dist_dir}}"
	GOOS=linux GOARCH=amd64 {{go}} build -ldflags "-X arcatum/pkg/version.Version={{version}}" -o "{{dist_dir}}/arcatum-runner-linux-amd64" ./cmd/runner
	GOOS=linux GOARCH=arm64 {{go}} build -ldflags "-X arcatum/pkg/version.Version={{version}}" -o "{{dist_dir}}/arcatum-runner-linux-arm64" ./cmd/runner
	printf '%s\n' "{{version}}" > "{{dist_dir}}/VERSION"

# Run all tests.
test:
	{{go}} test ./...

# Run tests with race detector.
test-race:
	{{go}} test -race ./...

# Run go vet.
vet:
	{{go}} vet ./...

# Format all Go sources in place.
fmt:
	{{gofmt}} -l -w .

# Everything that must pass before a change goes out: gofmt, vet, test, build.
check:
	unformatted=$({{gofmt}} -l .); if [[ -n "$unformatted" ]]; then echo "not gofmt'd:" >&2; echo "$unformatted" >&2; exit 1; fi
	{{go}} vet ./...
	{{go}} test ./...
	{{go}} build ./...

# Remove build outputs (bin/ and the local dist dir). Leaves local/data and local/backup alone.
clean:
	rm -rf bin local/dist

# Initialize local development files if missing.
dev-init:
	mkdir -p local/data local/backup
	if [[ ! -f local/server.toml ]]; then cp config/server.example.toml local/server.toml; sed -i '0,/^listen/s|^listen[[:space:]]*=.*|listen   = "{{listen}}"|' local/server.toml; sed -i 's|^listen[[:space:]]*=[[:space:]]*"0.0.0.0:8080"|listen      = "{{web_listen}}"|' local/server.toml; sed -i 's|^data_dir[[:space:]]*=.*|data_dir = "./local/data"|' local/server.toml; sed -i 's|^backup_dir[[:space:]]*=.*|backup_dir = "./local/backup"|' local/server.toml; fi
	if [[ ! -f local/instances.json ]]; then cp data/instances.example.json local/instances.json; sed -i "s|REPLACE-WITH-RUNNER-HOSTNAME|$(hostname -s)|g" local/instances.json; fi

# Run server in local dev mode.
server config=server_cfg instances_file=instances:
	{{go}} run ./cmd/server -config "{{config}}" -instances "{{instances_file}}"

# Set (or create) a web account's password; prints a generated one. Use ARCATUM_PASSWORD
# to choose it yourself, e.g. `ARCATUM_PASSWORD=tajneheslo just passwd petr`.
passwd user="admin" role="admin" config=server_cfg:
	{{go}} run ./cmd/server -config "{{config}}" -passwd "{{user}}" -passwd-role "{{role}}"

# Run runner once against local server.
runner-once url=server_url config="":
	if [[ -n "{{config}}" ]]; then {{go}} run ./cmd/runner -config "{{config}}" -server "{{url}}" -once; else {{go}} run ./cmd/runner -server "{{url}}" -once; fi

# Run runner as a polling service against local server.
runner url=server_url config="":
	if [[ -n "{{config}}" ]]; then {{go}} run ./cmd/runner -config "{{config}}" -server "{{url}}"; else {{go}} run ./cmd/runner -server "{{url}}"; fi

# Generate a local development PKI into local/pki.
dev-certs hosts="127.0.0.1" admin="dev":
	deploy/gen-certs.sh -d local/pki -H "{{hosts}}" -a "{{admin}}"

# Issue a runner certificate from the local PKI (defaults to this machine's hostname).
dev-runner-cert id="":
	{{go}} run ./cmd/arcatum-ca runner -dir local/pki -id "$(if [[ -n "{{id}}" ]]; then echo "{{id}}"; else hostname -s; fi)"

# Run arcatum-ca with arbitrary arguments, e.g. `just ca admin -dir local/pki -name dev`.
ca *args:
	{{go}} run ./cmd/arcatum-ca {{args}}

# Trigger an instance run via API.
trigger instance="hello-demo" url=server_url:
	curl -sS -X POST "{{url}}/api/v1/instances/{{instance}}/run"

# List runs.
runs url=server_url:
	curl -sS "{{url}}/api/v1/runs"

# Show output of a run. Takes "run-1" or just "1".
run-output run_id stream="stdout" url=server_url:
	id="{{run_id}}"; if [[ "$id" =~ ^[0-9]+$ ]]; then id="run-$id"; fi; curl -sS "{{url}}/api/v1/runs/$id/output?stream={{stream}}"

# Read output of a run from an offset — the increment the live tail uses.
run-tail run_id offset="0" stream="stdout" url=server_url:
	id="{{run_id}}"; if [[ "$id" =~ ^[0-9]+$ ]]; then id="run-$id"; fi; curl -sS "{{url}}/api/v1/runs/$id/tail?offset={{offset}}&stream={{stream}}"

# List instances with their next run.
instances url=server_url:
	curl -sS "{{url}}/api/v1/instances"

# List registered runners.
runners url=server_url:
	curl -sS "{{url}}/api/v1/runners"

# Show the text status page (includes the loaded script catalog).
status url=server_url:
	curl -sS "{{url}}/status"
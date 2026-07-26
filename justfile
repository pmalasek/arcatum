# backup_central task runner

set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

go := env_var_or_default("GO", "go")
version := env_var_or_default("V", "$(date +%Y.%m.%d)")
server_cfg := env_var_or_default("SERVER_CONFIG", "local/server.toml")
instances := env_var_or_default("INSTANCES", "local/instances.json")
server_url := env_var_or_default("SERVER_URL", "http://127.0.0.1:8443")

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

# Initialize local development files if missing.
dev-init:
	mkdir -p local/data local/backup
	if [[ ! -f local/server.toml ]]; then cp config/server.example.toml local/server.toml; sed -i 's|^listen[[:space:]]*=.*|listen   = "127.0.0.1:8443"|' local/server.toml; sed -i 's|^data_dir[[:space:]]*=.*|data_dir = "./local/data"|' local/server.toml; sed -i 's|^backup_dir[[:space:]]*=.*|backup_dir = "./local/backup"|' local/server.toml; fi
	if [[ ! -f local/instances.json ]]; then cp data/instances.example.json local/instances.json; fi

# Run server in local dev mode.
server config=server_cfg instances_file=instances:
	{{go}} run ./cmd/server -config "{{config}}" -instances "{{instances_file}}"

# Run runner once against local server.
runner-once url=server_url config="":
	if [[ -n "{{config}}" ]]; then {{go}} run ./cmd/runner -config "{{config}}" -server "{{url}}" -once; else {{go}} run ./cmd/runner -server "{{url}}" -once; fi

# Trigger an instance run via API.
trigger instance="hello-demo" url=server_url:
	curl -sS -X POST "{{url}}/api/v1/instances/{{instance}}/run"

# List runs.
runs url=server_url:
	curl -sS "{{url}}/api/v1/runs"

# Show output of a run.
run-output run_id url=server_url:
	curl -sS "{{url}}/api/v1/runs/{{run_id}}/output"
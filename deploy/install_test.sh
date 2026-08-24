#!/bin/sh
set -eu

script_dir=$(
  CDPATH=''
  cd -- "$(dirname -- "$0")"
  pwd
)
project_dir=$(dirname -- "$script_dir")
installer="$project_dir/install.sh"
tmpdir=$(mktemp -d)

cleanup() {
  find "$tmpdir" -depth -delete
}
trap cleanup EXIT INT TERM

mkdir -p "$tmpdir/bin"
mock_docker="$tmpdir/bin/docker"
cat > "$mock_docker" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$MOCK_DOCKER_LOG"
if [ "${1:-}" = network ] && [ "${2:-}" = inspect ]; then
  exit 1
fi
exit 0
EOF
chmod +x "$mock_docker"

assert_env_nonempty() {
  file="$1"
  key="$2"
  if ! grep -Eq "^${key}='[^']+'$" "$file"; then
    printf 'missing non-empty %s in %s\n' "$key" "$file" >&2
    exit 1
  fi
}

embedded_env="$tmpdir/embedded.env"
embedded_log="$tmpdir/embedded-docker.log"
env PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$embedded_log" \
  ONE_SEARCH_ENV_FILE="$embedded_env" \
  "$installer" --mode embedded --yes --no-build --timeout 1 > "$tmpdir/embedded-output"

assert_env_nonempty "$embedded_env" POSTGRES_PASSWORD
assert_env_nonempty "$embedded_env" ADMIN_PASSWORD
assert_env_nonempty "$embedded_env" ENCRYPTION_KEY
grep -q "^ONE_SEARCH_INSTALL_MODE='embedded'$" "$embedded_env"
grep -q 'compose .*docker-compose.yml up -d' "$embedded_log"
if grep -q 'docker-compose.external-db.yml' "$embedded_log"; then
  printf '%s\n' 'embedded install used external compose' >&2
  exit 1
fi

encryption_before=$(grep '^ENCRYPTION_KEY=' "$embedded_env")
env PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$embedded_log" \
  ONE_SEARCH_ENV_FILE="$embedded_env" \
  "$installer" --yes --no-build --timeout 1 > "$tmpdir/embedded-rerun-output"
encryption_after=$(grep '^ENCRYPTION_KEY=' "$embedded_env")
[ "$encryption_before" = "$encryption_after" ] || {
  printf '%s\n' 'rerun replaced ENCRYPTION_KEY' >&2
  exit 1
}

external_env="$tmpdir/external.env"
external_log="$tmpdir/external-docker.log"
database_url='postgres://one_search:secret@shared-postgres:5432/one_search?sslmode=disable'
printf '%s\n' "$database_url" > "$tmpdir/database-url"
(
  cd "$tmpdir"
  env PATH="$tmpdir/bin:$PATH" \
    MOCK_DOCKER_LOG="$external_log" \
    ONE_SEARCH_ENV_FILE="$external_env" \
    "$installer" --mode external --database-url-file database-url \
    --database-network shared-db --yes --no-build --timeout 1 > "$tmpdir/external-output"
)

grep -q "^DATABASE_URL='$database_url'$" "$external_env"
grep -q "^ONE_SEARCH_USE_SHARED_DB_NETWORK='true'$" "$external_env"
grep -q '^network create --internal shared-db$' "$external_log"
grep -q 'docker-compose.external-db.yml .*docker-compose.shared-db.yml up -d' "$external_log"
grep -q 'docker-compose.external-db.yml .*docker-compose.shared-db.yml config --quiet' "$external_log"
if grep -q "$database_url" "$tmpdir/external-output" "$external_log"; then
  printf '%s\n' 'installer leaked DATABASE_URL' >&2
  exit 1
fi

external_rerun_log="$tmpdir/external-rerun-docker.log"
env PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$external_rerun_log" \
  ONE_SEARCH_ENV_FILE="$external_env" \
  "$installer" --yes --no-build --timeout 1 > "$tmpdir/external-rerun-output"
grep -q 'docker-compose.external-db.yml .*docker-compose.shared-db.yml up -d' "$external_rerun_log"

missing_env="$tmpdir/missing.env"
if env PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$tmpdir/missing-docker.log" \
  ONE_SEARCH_ENV_FILE="$missing_env" \
  "$installer" --mode external --yes --no-build --timeout 1 >/dev/null 2>&1; then
  printf '%s\n' 'external install accepted a missing DATABASE_URL' >&2
  exit 1
fi

invalid_network_env="$tmpdir/invalid-network.env"
if env PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$tmpdir/invalid-network-docker.log" \
  ONE_SEARCH_ENV_FILE="$invalid_network_env" \
  DATABASE_URL="$database_url" \
  "$installer" --mode external --database-network '../bad' --yes --no-build --timeout 1 >/dev/null 2>&1; then
  printf '%s\n' 'installer accepted an invalid network name' >&2
  exit 1
fi

symlink_target="$tmpdir/symlink-target.env"
symlink_env="$tmpdir/symlink.env"
printf '%s\n' 'sentinel' > "$symlink_target"
ln -s "$symlink_target" "$symlink_env"
if env PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$tmpdir/symlink-docker.log" \
  ONE_SEARCH_ENV_FILE="$symlink_env" \
  "$installer" --yes --no-build --timeout 1 >/dev/null 2>&1; then
  printf '%s\n' 'installer accepted a symlink env file' >&2
  exit 1
fi
grep -q '^sentinel$' "$symlink_target"

switch_log="$tmpdir/switch-docker.log"
env PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$switch_log" \
  ONE_SEARCH_ENV_FILE="$external_env" \
  "$installer" --mode embedded --yes --no-build --timeout 1 > "$tmpdir/switch-output"
grep -q "^ONE_SEARCH_INSTALL_MODE='embedded'$" "$external_env"
grep -q "^ONE_SEARCH_USE_SHARED_DB_NETWORK='false'$" "$external_env"

duplicate_env="$tmpdir/duplicate.env"
cp "$project_dir/.env.example" "$duplicate_env"
printf '%s\n' 'ENCRYPTION_KEY=duplicate' >> "$duplicate_env"
if env PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$tmpdir/duplicate-docker.log" \
  ONE_SEARCH_ENV_FILE="$duplicate_env" \
  "$installer" --yes --no-build --timeout 1 >/dev/null 2>&1; then
  printf '%s\n' 'installer accepted duplicate managed keys' >&2
  exit 1
fi

printf '%s\n' 'installer tests passed'

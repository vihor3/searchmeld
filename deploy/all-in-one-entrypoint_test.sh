#!/bin/sh
# shellcheck disable=SC2016
set -eu

if [ "${GITHUB_ACTIONS:-}" != true ]; then
  printf '%s\n' 'Entrypoint checks may run only in GitHub Actions' >&2
  exit 1
fi

script_dir=$(
  CDPATH=''
  cd -- "$(dirname -- "$0")"
  pwd
)
entrypoint="$script_dir/all-in-one-entrypoint.sh"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

assert_mode() {
  expected="$1"
  shift
  actual=$(
    env SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true "$@" sh -c \
      '. "$1"; select_database_mode; printf "%s\n" "$database_mode"' sh "$entrypoint" |
      tail -n 1
  )
  if [ "$actual" != "$expected" ]; then
    printf 'mode mismatch: got %s, want %s\n' "$actual" "$expected" >&2
    exit 1
  fi
}

assert_mode external env DATABASE_URL='postgres://app:secret@db:5432/app' SEARCHMELD_DEFAULT_DATABASE_MODE=external
assert_mode embedded env DATABASE_URL= DATABASE_URL_FILE= SEARCHMELD_EMBEDDED_POSTGRES=true SEARCHMELD_DEFAULT_DATABASE_MODE=embedded
assert_mode embedded env DATABASE_URL='postgres://legacy-dev@localhost:15432/app' SEARCHMELD_EMBEDDED_POSTGRES=true SEARCHMELD_DEFAULT_DATABASE_MODE=embedded
assert_mode external env DATABASE_MODE=external DATABASE_URL='postgres://app:secret@db:5432/app' SEARCHMELD_EMBEDDED_POSTGRES=true SEARCHMELD_DEFAULT_DATABASE_MODE=embedded

legacy_mode=$(
  env ONE_SEARCH_ENTRYPOINT_SKIP_MAIN=true \
    DATABASE_URL= DATABASE_URL_FILE= \
    ONE_SEARCH_EMBEDDED_POSTGRES=true \
    ONE_SEARCH_DEFAULT_DATABASE_MODE=embedded \
    sh -c '. "$1"; select_database_mode; printf "%s\n" "$database_mode"' sh "$entrypoint" |
    tail -n 1
)
if [ "$legacy_mode" != embedded ]; then
  printf 'legacy mode mismatch: got %s, want embedded\n' "$legacy_mode" >&2
  exit 1
fi

escaped=$(
  env SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true sh -c \
    '. "$1"; escape_conninfo_value "$2"' sh "$entrypoint" "a/b:c@d?e#f+g=h\\i'j"
)
if [ "$escaped" != "a/b:c@d?e#f+g=h\\\\i\\'j" ]; then
  printf 'conninfo escaping mismatch: %s\n' "$escaped" >&2
  exit 1
fi

printf '%s\n' 'postgres://app:file-secret@db:5432/app' > "$tmpdir/database-url"
assert_mode external env DATABASE_URL= DATABASE_URL_FILE="$tmpdir/database-url" SEARCHMELD_DEFAULT_DATABASE_MODE=external SEARCHMELD_EMBEDDED_POSTGRES=false

if env DATABASE_URL= DATABASE_URL_FILE= \
  SEARCHMELD_DEFAULT_DATABASE_MODE=external \
  SEARCHMELD_EMBEDDED_POSTGRES=false \
  SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true \
  sh -c '. "$1"; select_database_mode' sh "$entrypoint" >/dev/null 2>&1; then
  printf '%s\n' 'external image accepted a missing DATABASE_URL' >&2
  exit 1
fi

if env DATABASE_MODE=embedded \
  SEARCHMELD_EMBEDDED_POSTGRES=false \
  SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true \
  sh -c '. "$1"; select_database_mode' sh "$entrypoint" >/dev/null 2>&1; then
  printf '%s\n' 'external image accepted DATABASE_MODE=embedded' >&2
  exit 1
fi

assert_pgdata_rejected() {
  directory=$1
  expected_message=$2
  : > "$tmpdir/mutations"
  if env SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true PGDATA="$directory" \
    POSTGRES_PASSWORD=synthetic-test-password MUTATIONS="$tmpdir/mutations" \
    TEST_POSTGRES_VERSION="${3:-16.15}" \
    sh -c '
      . "$1"
      postgres() { printf "postgres (PostgreSQL) %s\n" "$TEST_POSTGRES_VERSION"; }
      mutation() { printf "%s\n" "$1" >> "$MUTATIONS"; exit 97; }
      mkdir() { mutation mkdir; }
      chown() { mutation chown; }
      initdb() { mutation initdb; }
      pg_isready() { mutation pg_isready; }
      psql() { mutation psql; }
      su-exec() { mutation su-exec; }
      prepare_embedded_postgres
    ' sh "$entrypoint" > "$tmpdir/rejection" 2>&1; then
    printf '%s\n' 'unsafe PGDATA was accepted' >&2
    exit 1
  fi
  if [ -s "$tmpdir/mutations" ] || ! grep -Fq "$expected_message" "$tmpdir/rejection"; then
    printf '%s\n' 'PGDATA preflight failed to reject before mutation with the expected message' >&2
    exit 1
  fi
}

mkdir "$tmpdir/pgdata"
for major in 15 17; do
  printf '%s\n' "$major" > "$tmpdir/pgdata/PG_VERSION"
  assert_pgdata_rejected "$tmpdir/pgdata" 'incompatible PG_VERSION'
  if [ "$(cat "$tmpdir/pgdata/PG_VERSION")" != "$major" ]; then
    printf '%s\n' 'rejected PG_VERSION changed' >&2
    exit 1
  fi
done

: > "$tmpdir/pgdata/PG_VERSION"
assert_pgdata_rejected "$tmpdir/pgdata" 'invalid PG_VERSION'
printf '16\n17\n' > "$tmpdir/pgdata/PG_VERSION"
assert_pgdata_rejected "$tmpdir/pgdata" 'invalid PG_VERSION'
printf '%s\n' xx > "$tmpdir/pgdata/PG_VERSION"
assert_pgdata_rejected "$tmpdir/pgdata" 'incompatible PG_VERSION'
printf '16\000' > "$tmpdir/pgdata/PG_VERSION"
assert_pgdata_rejected "$tmpdir/pgdata" 'incompatible PG_VERSION'
rm "$tmpdir/pgdata/PG_VERSION"
mkdir "$tmpdir/pgdata/PG_VERSION"
assert_pgdata_rejected "$tmpdir/pgdata" 'readable regular file'
rmdir "$tmpdir/pgdata/PG_VERSION"
ln -s "$tmpdir/missing-marker" "$tmpdir/pgdata/PG_VERSION"
assert_pgdata_rejected "$tmpdir/pgdata" 'readable regular file'
rm "$tmpdir/pgdata/PG_VERSION"
printf '%s\n' 'synthetic persisted bytes' > "$tmpdir/pgdata/.sentinel"
assert_pgdata_rejected "$tmpdir/pgdata" 'has no PG_VERSION'
assert_pgdata_rejected "$tmpdir/pgdata/.sentinel" 'not a directory'
printf '%s\n' 16 > "$tmpdir/pgdata/PG_VERSION"
assert_pgdata_rejected "$tmpdir/pgdata" 'binary must be major 16' 17.7

# Existing compatible data may be prepared, but must never be initialized again.
env SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true PGDATA="$tmpdir/pgdata" \
  POSTGRES_PASSWORD=synthetic-test-password \
  sh -c '
    . "$1"
    postgres() { printf "postgres (PostgreSQL) 16.15\n"; }
    mkdir() { :; }
    chown() { :; }
    initdb() { exit 97; }
    pg_isready() { exit 97; }
    psql() { exit 97; }
    su-exec() { exit 97; }
    prepare_embedded_postgres
  ' sh "$entrypoint"

# An absent or empty PGDATA is permitted by the read-only preflight.
mkdir "$tmpdir/empty-pgdata"
for directory in "$tmpdir/absent-pgdata" "$tmpdir/empty-pgdata"; do
  env SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true PGDATA="$directory" \
    sh -c '
      . "$1"
      postgres() { printf "postgres (PostgreSQL) 16.15\n"; }
      validate_embedded_pgdata
    ' sh "$entrypoint"
done

mkdir "$tmpdir/init-password-files"
if env SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true PGDATA="$tmpdir/empty-pgdata" \
  POSTGRES_PASSWORD=synthetic-test-password TMPDIR="$tmpdir/init-password-files" \
  sh -c '
    . "$1"
    trap cleanup EXIT
    postgres() { printf "postgres (PostgreSQL) 16.15\n"; }
    mkdir() { :; }
    chown() { :; }
    initdb() { exit 97; }
    pg_isready() { exit 97; }
    psql() { exit 97; }
    su-exec() { exit 97; }
    timeout() {
      [ "$1 $2 $3 $4 $5 $6 $7 $8" = "-s TERM -k 5 120 su-exec postgres initdb" ] || exit 97
      return 124
    }
    prepare_embedded_postgres
  ' sh "$entrypoint" > "$tmpdir/init-result" 2>&1; then
  printf '%s\n' 'timed-out initialization was accepted' >&2
  exit 1
fi
if ! grep -Fq 'postgres initialization failed or exceeded its startup timeout' "$tmpdir/init-result"; then
  printf '%s\n' 'initialization timeout did not report failure' >&2
  exit 1
fi
for password_file in "$tmpdir/init-password-files"/*; do
  if [ -e "$password_file" ]; then
    printf '%s\n' 'initialization failure left a password file' >&2
    exit 1
  fi
done

if env SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true POSTGRES_USER=synthetic-test-user \
  POSTGRES_DB=synthetic-test-db POSTGRES_PASSWORD=synthetic-test-password \
  sh -c '
    . "$1"
    psql_admin() {
      case "$*" in
        *"ALTER ROLE"*) return 0 ;;
        *"SELECT 1 FROM pg_database"*) return 1 ;;
        *) exit 97 ;;
      esac
    }
    ensure_database
  ' sh "$entrypoint" > "$tmpdir/database-result" 2>&1; then
  printf '%s\n' 'failed database lookup was accepted' >&2
  exit 1
fi
if ! grep -Fq 'failed to check whether the application database exists' "$tmpdir/database-result"; then
  printf '%s\n' 'database lookup failure was not propagated' >&2
  exit 1
fi

assert_readiness_failure() {
  component=$1
  alive=$2
  expected=$3
  calls=$4
  : > "$tmpdir/probes"
  if env SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true COMPONENT="$component" \
    ALIVE="$alive" PROBES="$tmpdir/probes" POSTGRES_USER=synthetic-test-user \
    sh -c '
      . "$1"
      postgres_pid=12345
      backend_pid=12346
      pg_isready() {
        [ "$1" = -t ] && [ "$2" = 1 ] || exit 97
        printf "probe\n" >> "$PROBES"
        return 1
      }
      curl() {
        case " $* " in
          *" --connect-timeout 1 --max-time 1 "*) ;;
          *) exit 97 ;;
        esac
        printf "probe\n" >> "$PROBES"
        return 1
      }
      kill() { [ "$ALIVE" = true ]; }
      sleep() { :; }
      case "$COMPONENT" in
        postgres) wait_for_postgres ;;
        backend) wait_for_backend ;;
      esac
    ' sh "$entrypoint" > "$tmpdir/readiness" 2>&1; then
    printf '%s\n' 'unhealthy process was accepted' >&2
    exit 1
  fi
  if ! grep -Fq "$expected" "$tmpdir/readiness" || [ "$(wc -l < "$tmpdir/probes")" -ne "$calls" ]; then
    printf '%s\n' 'readiness failure did not preserve its deadline and process-exit checks' >&2
    exit 1
  fi
}

assert_readiness_failure postgres true 'postgres did not become ready in time' 60
assert_readiness_failure postgres false 'postgres exited during startup' 1
assert_readiness_failure backend true 'backend did not become healthy in time' 120
assert_readiness_failure backend false 'backend exited before becoming healthy' 1

env SEARCHMELD_ENTRYPOINT_SKIP_MAIN=true POSTGRES_USER=synthetic-test-user \
  sh -c '
    . "$1"
    pg_isready() { return 0; }
    curl() { return 0; }
    wait_for_postgres
    wait_for_backend
  ' sh "$entrypoint"

printf '%s\n' 'entrypoint mode, PGDATA safety and bounded readiness tests passed'

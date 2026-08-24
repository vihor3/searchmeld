#!/bin/sh
set -eu

log() {
  printf '%s\n' "$*"
}

escape_sql_literal() {
  printf "%s" "$1" | sed "s/'/''/g"
}

escape_sql_ident() {
  printf '"%s"' "$(printf "%s" "$1" | sed 's/"/""/g')"
}

escape_conninfo_value() {
  printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e "s/'/\\\\'/g"
}

normalize_container_proxy_env() {
  normalize_one_proxy HTTP_PROXY
  normalize_one_proxy HTTPS_PROXY
  normalize_one_proxy ALL_PROXY
  normalize_one_proxy http_proxy
  normalize_one_proxy https_proxy
  normalize_one_proxy all_proxy
}

normalize_one_proxy() {
  name="$1"
  value=$(eval "printf '%s' \"\${$name:-}\"")
  if [ -z "$value" ]; then
    return
  fi
  value=$(printf '%s' "$value" | sed 's#//127\.0\.0\.1:#//host.docker.internal:#; s#//localhost:#//host.docker.internal:#')
  eval "export $name=\"$value\""
}

psql_admin() {
  su-exec postgres psql -U "$POSTGRES_USER" -d postgres -v ON_ERROR_STOP=1 "$@"
}

psql_admin_scalar() {
  su-exec postgres psql -U "$POSTGRES_USER" -d postgres -tAc "$1" | tr -d '[:space:]'
}

cleanup() {
  trap - INT TERM EXIT
  for pid in ${nginx_pid:-} ${backend_pid:-} ${postgres_pid:-}; do
    if [ -n "${pid:-}" ] && kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done
}

load_database_url() {
  if [ -n "${DATABASE_URL:-}" ]; then
    return
  fi
  if [ -z "${DATABASE_URL_FILE:-}" ]; then
    return
  fi
  if [ ! -r "$DATABASE_URL_FILE" ]; then
    log "DATABASE_URL_FILE is not readable: $DATABASE_URL_FILE"
    exit 1
  fi
  DATABASE_URL=$(tr -d '\r\n' < "$DATABASE_URL_FILE")
  if [ -z "$DATABASE_URL" ]; then
    log "DATABASE_URL_FILE is empty: $DATABASE_URL_FILE"
    exit 1
  fi
  export DATABASE_URL
}

select_database_mode() {
  requested_mode=$(printf '%s' "${DATABASE_MODE:-${ONE_SEARCH_DEFAULT_DATABASE_MODE:-auto}}" | tr '[:upper:]' '[:lower:]')
  case "$requested_mode" in
    external)
      load_database_url
      if [ -z "${DATABASE_URL:-}" ]; then
        log "DATABASE_URL or DATABASE_URL_FILE is required when DATABASE_MODE=external"
        exit 1
      fi
      database_mode="external"
      ;;
    embedded)
      if [ "${ONE_SEARCH_EMBEDDED_POSTGRES:-false}" != "true" ]; then
        log "DATABASE_MODE=embedded requires the all-in-one image"
        exit 1
      fi
      if [ -n "${DATABASE_URL:-}${DATABASE_URL_FILE:-}" ]; then
        log "external database settings are ignored because DATABASE_MODE=embedded"
      fi
      database_mode="embedded"
      ;;
    auto)
      load_database_url
      if [ -n "${DATABASE_URL:-}" ]; then
        database_mode="external"
      elif [ "${ONE_SEARCH_EMBEDDED_POSTGRES:-false}" = "true" ]; then
        database_mode="embedded"
      else
        log "DATABASE_URL or DATABASE_URL_FILE is required by the external image"
        exit 1
      fi
      ;;
    *)
      log "DATABASE_MODE must be embedded, external, or auto"
      exit 1
      ;;
  esac
}

prepare_embedded_postgres() {
  for binary in postgres initdb pg_isready psql su-exec; do
    if ! command -v "$binary" >/dev/null 2>&1; then
      log "embedded PostgreSQL support is unavailable: missing $binary"
      exit 1
    fi
  done

  : "${PGDATA:=/var/lib/postgresql/data}"
  : "${POSTGRES_DB:=one_search}"
  : "${POSTGRES_USER:=one_search}"
  : "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required when using the embedded database}"

  mkdir -p "$PGDATA" /run/postgresql
  chown -R postgres:postgres "$PGDATA" /run/postgresql

  if [ ! -s "$PGDATA/PG_VERSION" ]; then
    log "initializing postgres data directory"
    pwfile=$(mktemp)
    printf '%s\n' "$POSTGRES_PASSWORD" > "$pwfile"
    chown postgres:postgres "$pwfile"
    su-exec postgres initdb \
      -D "$PGDATA" \
      --username="$POSTGRES_USER" \
      --pwfile="$pwfile" \
      --auth-local=trust \
      --auth-host=scram-sha-256 \
      >/proc/1/fd/1 2>&1
    rm -f "$pwfile"
  fi
}

wait_for_backend() {
  attempts=0
  while ! curl --noproxy '*' -fsS http://127.0.0.1:8080/healthz >/dev/null 2>&1; do
    if ! kill -0 "$backend_pid" 2>/dev/null; then
      log "backend exited before becoming healthy"
      exit 1
    fi
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 120 ]; then
      log "backend did not become healthy in time"
      exit 1
    fi
    sleep 1
  done
}

start_postgres() {
  log "starting postgres"
  su-exec postgres postgres \
    -D "$PGDATA" \
    -p 5432 \
    -c listen_addresses=127.0.0.1 \
    -c logging_collector=off \
    -c log_destination=stderr \
    -c client_min_messages=warning \
    >/proc/1/fd/1 2>&1 &
  postgres_pid=$!

  until pg_isready -U "$POSTGRES_USER" >/dev/null 2>&1; do
    if ! kill -0 "$postgres_pid" 2>/dev/null; then
      log "postgres exited during startup"
      exit 1
    fi
    sleep 1
  done
  log "postgres is ready"
}

ensure_database() {
  app_db_lit=$(escape_sql_literal "$POSTGRES_DB")
  app_pass_lit=$(escape_sql_literal "$POSTGRES_PASSWORD")
  app_db_ident=$(escape_sql_ident "$POSTGRES_DB")
  app_user_ident=$(escape_sql_ident "$POSTGRES_USER")

  log "ensuring database and role"
  psql_admin -c "ALTER ROLE $app_user_ident WITH LOGIN PASSWORD '$app_pass_lit';"

  if [ "$(psql_admin_scalar "SELECT 1 FROM pg_database WHERE datname = '$app_db_lit'")" != "1" ]; then
    psql_admin -c "CREATE DATABASE $app_db_ident OWNER $app_user_ident;"
  else
    psql_admin -c "ALTER DATABASE $app_db_ident OWNER TO $app_user_ident;"
  fi
}

start_backend() {
  export APP_ENV="${APP_ENV:-production}"
  export HTTP_ADDR="${HTTP_ADDR:-:8080}"
  export MIGRATIONS_DIR="${MIGRATIONS_DIR:-/app/backend/migrations}"
  export RUN_MIGRATIONS="${RUN_MIGRATIONS:-true}"
  export ADMIN_USERNAME="${ADMIN_USERNAME:-admin}"
  export ADMIN_PASSWORD="${ADMIN_PASSWORD:-}"
  : "${ENCRYPTION_KEY:?ENCRYPTION_KEY is required and must be at least 32 characters}"
  export ENCRYPTION_KEY
  export API_AUTH_REQUIRED="${API_AUTH_REQUIRED:-true}"
  export MCP_ENABLED="${MCP_ENABLED:-false}"
  export MCP_PATH="${MCP_PATH:-/mcp}"
  export CORS_ALLOWED_ORIGINS="${CORS_ALLOWED_ORIGINS:-http://localhost:5173,http://localhost:8080}"
  export UPSTREAM_USER_AGENT="${UPSTREAM_USER_AGENT:-OneSearchRelay/0.1}"
  export REQUEST_TIMEOUT_MS="${REQUEST_TIMEOUT_MS:-20000}"
  if [ "$database_mode" = "embedded" ]; then
    database_user=$(escape_conninfo_value "$POSTGRES_USER")
    database_password=$(escape_conninfo_value "$POSTGRES_PASSWORD")
    database_name=$(escape_conninfo_value "$POSTGRES_DB")
    export DATABASE_URL="host=127.0.0.1 port=5432 user='$database_user' password='$database_password' dbname='$database_name' sslmode=disable"
  fi

  log "starting backend on ${HTTP_ADDR} with ${database_mode} database"
  /usr/local/bin/one-search &
  backend_pid=$!

  wait_for_backend
  log "backend is healthy"
}

start_nginx() {
  log "starting nginx"
  nginx -g 'daemon off;' &
  nginx_pid=$!
}

main() {
  trap 'cleanup; exit 0' INT TERM
  trap cleanup EXIT

  normalize_container_proxy_env
  select_database_mode

  if [ "$database_mode" = "embedded" ]; then
    prepare_embedded_postgres
    start_postgres
    ensure_database
  else
    log "using configured external PostgreSQL"
  fi
  start_backend
  start_nginx

  log "one-search stack is ready"

  while true; do
    if [ "$database_mode" = "embedded" ] && ! kill -0 "$postgres_pid" 2>/dev/null; then
      log "postgres stopped unexpectedly"
      exit 1
    fi
    if ! kill -0 "$backend_pid" 2>/dev/null; then
      log "backend stopped unexpectedly"
      exit 1
    fi
    if ! kill -0 "$nginx_pid" 2>/dev/null; then
      log "nginx stopped unexpectedly"
      exit 1
    fi
    sleep 5
  done
}

if [ "${ONE_SEARCH_ENTRYPOINT_SKIP_MAIN:-false}" != "true" ]; then
  main "$@"
fi

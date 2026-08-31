#!/bin/sh
# shellcheck disable=SC2016
set -eu

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

printf '%s\n' 'entrypoint mode tests passed'

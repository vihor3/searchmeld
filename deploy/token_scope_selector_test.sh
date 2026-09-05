#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(dirname -- "$script_dir")
frontend_dir=${FRONTEND_DIR:-$project_dir/frontend}
view_file="$frontend_dir/src/views/TokensView.vue"
main_file="$frontend_dir/src/main.ts"

[ -f "$view_file" ] || {
  printf 'token view not found: %s\n' "$view_file" >&2
  exit 1
}

checkbox_count=$(grep -c 'v-model="form.scopes" type="checkbox"' "$view_file" || true)
[ "$checkbox_count" -eq 2 ] || {
  printf 'expected two native scope checkboxes, found %s\n' "$checkbox_count" >&2
  exit 1
}

grep -Fq 'type="checkbox" value="search"' "$view_file" || {
  printf '%s\n' 'search scope checkbox is missing' >&2
  exit 1
}
grep -Fq 'type="checkbox" value="extract"' "$view_file" || {
  printf '%s\n' 'extract scope checkbox is missing' >&2
  exit 1
}
grep -Fq "form.scopes.includes('search')" "$view_file" || {
  printf '%s\n' 'search selected state is missing' >&2
  exit 1
}
grep -Fq "form.scopes.includes('extract')" "$view_file" || {
  printf '%s\n' 'extract selected state is missing' >&2
  exit 1
}

if grep -Eq '<el-checkbox(-group|-button)?' "$view_file"; then
  printf '%s\n' 'unresolved Element Plus checkbox tags remain in token view' >&2
  exit 1
fi
if grep -Eq 'ElCheckbox(Button|Group)' "$main_file"; then
  printf '%s\n' 'unused Element Plus checkbox registration remains' >&2
  exit 1
fi

printf '%s\n' 'token scope selector check passed'

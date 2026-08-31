#!/bin/sh
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(dirname -- "$script_dir")
frontend_dir=${FRONTEND_DIR:-$project_dir/frontend}
main_file="$frontend_dir/src/main.ts"

[ -f "$main_file" ] || {
  printf 'frontend entry not found: %s\n' "$main_file" >&2
  exit 1
}

component_tags=$(
  find "$frontend_dir/src" -type f -name '*.vue' -exec grep -hoE '<el-[a-z0-9-]+' {} + 2>/dev/null |
    sed 's/^<//' |
    sort -u
)

registered_components=$(
  sed -n '/^;\[$/,/^\]\.forEach/p' "$main_file" |
    sed -n 's/^[[:space:]]*\(El[A-Za-z0-9]*\),\{0,1\}[[:space:]]*$/\1/p'
)

missing=false
for tag in $component_tags; do
  component=$(
    printf '%s\n' "$tag" | awk -F- '{
      printf "El"
      for (segment_index = 2; segment_index <= NF; segment_index++) {
        printf "%s%s", toupper(substr($segment_index, 1, 1)), substr($segment_index, 2)
      }
      printf "\n"
    }'
  )
  if ! printf '%s\n' "$registered_components" | grep -qx "$component"; then
    printf 'Element Plus template component is not registered: %s (%s)\n' "$component" "$tag" >&2
    missing=true
  fi
done

[ "$missing" = false ] || exit 1
printf '%s\n' 'frontend component registration check passed'

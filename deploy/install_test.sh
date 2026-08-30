#!/bin/sh
set -eu

script_dir=$(
  CDPATH=''
  cd -- "$(dirname -- "$0")"
  pwd
)
project_dir=$(dirname -- "$script_dir")
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

make_project() {
  name="$1"
  target="$tmpdir/$name"
  mkdir -p "$target"
  cp "$project_dir/install.sh" "$target/install.sh"
  cp "$project_dir/.env.example" "$target/.env.example"
  cp "$project_dir/docker-compose.yml" "$target/docker-compose.yml"
  cp "$project_dir/docker-compose.external-db.yml" "$target/docker-compose.external-db.yml"
  cp "$project_dir/docker-compose.shared-db.yml" "$target/docker-compose.shared-db.yml"
  printf '%s' "$target"
}

assert_env_nonempty() {
  file="$1"
  key="$2"
  if ! grep -Eq "^${key}='[^']+'$" "$file"; then
    printf 'missing non-empty %s in %s\n' "$key" "$file" >&2
    exit 1
  fi
}

# 首次运行：即使调用者设置了同名环境变量，也只能由向导决定配置。
embedded_project=$(make_project embedded)
embedded_log="$tmpdir/embedded-docker.log"
printf '\n\n\n\n\n\n' | env \
  PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$embedded_log" \
  ONE_SEARCH_INSTALL_MODE=external \
  DATABASE_URL='postgresql://ignored:ignored@ignored:5432/ignored' \
  HOST_PORT=65500 \
  "$embedded_project/install.sh" > "$tmpdir/embedded-output" 2>&1

embedded_env="$embedded_project/.env"
assert_env_nonempty "$embedded_env" POSTGRES_PASSWORD
assert_env_nonempty "$embedded_env" ADMIN_PASSWORD
assert_env_nonempty "$embedded_env" ENCRYPTION_KEY
grep -q "^ONE_SEARCH_INSTALL_MODE='embedded'$" "$embedded_env"
grep -q "^HOST_PORT='5173'$" "$embedded_env"
grep -q 'compose .*docker-compose.yml up --build -d' "$embedded_log"
if grep -q 'docker-compose.external-db.yml' "$embedded_log"; then
  printf '%s\n' 'embedded install used external compose' >&2
  exit 1
fi

# 重复运行默认沿用现有配置，且不会替换持久化密钥。
encryption_before=$(grep '^ENCRYPTION_KEY=' "$embedded_env")
printf '\n\n' | env \
  PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$embedded_log" \
  "$embedded_project/install.sh" > "$tmpdir/embedded-rerun-output" 2>&1
encryption_after=$(grep '^ENCRYPTION_KEY=' "$embedded_env")
[ "$encryption_before" = "$encryption_after" ] || {
  printf '%s\n' 'rerun replaced ENCRYPTION_KEY' >&2
  exit 1
}

# 外部数据库逐项输入：验证 URL 编码、共享网络和敏感信息不泄漏。
external_project=$(make_project external)
external_log="$tmpdir/external-docker.log"
database_password='s@cr et/#?'
expected_database_url='postgresql://one_search:s%40cr%20et%2F%23%3F@shared-postgres:5432/one_search?sslmode=disable'
{
  printf '%s\n' 2
  printf '%s\n' y
  printf '\n'
  printf '%s\n' 1
  printf '\n'
  printf '\n'
  printf '\n'
  printf '\n'
  printf '%s\n' "$database_password"
  printf '%s\n' "$database_password"
  printf '\n'
  printf '%s\n' 5180
  printf '\n'
  printf '\n'
  printf '\n'
  printf '\n'
} | env \
  PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$external_log" \
  "$external_project/install.sh" > "$tmpdir/external-output" 2>&1

external_env="$external_project/.env"
grep -q "^DATABASE_URL='$expected_database_url'$" "$external_env"
grep -q "^ONE_SEARCH_INSTALL_MODE='external'$" "$external_env"
grep -q "^ONE_SEARCH_USE_SHARED_DB_NETWORK='true'$" "$external_env"
grep -q "^HOST_PORT='5180'$" "$external_env"
grep -q '^network create --internal shared-db$' "$external_log"
grep -q 'docker-compose.external-db.yml .*docker-compose.shared-db.yml config --quiet' "$external_log"
grep -q 'docker-compose.external-db.yml .*docker-compose.shared-db.yml up --build -d' "$external_log"
if grep -q "$database_password\|$expected_database_url" "$tmpdir/external-output" "$external_log"; then
  printf '%s\n' 'installer leaked external database credentials' >&2
  exit 1
fi

# 外部配置重复运行仍选择原来的 Compose 文件组合。
external_rerun_log="$tmpdir/external-rerun-docker.log"
printf '\n\n' | env \
  PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$external_rerun_log" \
  "$external_project/install.sh" > "$tmpdir/external-rerun-output" 2>&1
grep -q 'docker-compose.external-db.yml .*docker-compose.shared-db.yml up --build -d' "$external_rerun_log"

# 重新配置可从外部数据库切回内置数据库。
switch_log="$tmpdir/switch-docker.log"
{
  printf '%s\n' 2
  printf '%s\n' 1
  printf '\n'
  printf '\n'
  printf '\n'
  printf '\n'
} | env \
  PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$switch_log" \
  "$external_project/install.sh" > "$tmpdir/switch-output" 2>&1
grep -q "^ONE_SEARCH_INSTALL_MODE='embedded'$" "$external_env"
grep -q "^ONE_SEARCH_USE_SHARED_DB_NETWORK='false'$" "$external_env"
grep -q "^DATABASE_URL=''$" "$external_env"
assert_env_nonempty "$external_env" POSTGRES_PASSWORD

# 高级用户仍可在向导中粘贴完整 URL，但不能通过参数或环境变量注入。
url_project=$(make_project external-url)
url_log="$tmpdir/external-url-docker.log"
database_url='postgres://one_search:secret@db.example.com:5432/one_search?sslmode=require'
{
  printf '%s\n' 2
  printf '%s\n' n
  printf '%s\n' 2
  printf '%s\n' "$database_url"
  printf '\n'
  printf '\n'
  printf '\n'
  printf '%s\n' n
  printf '\n'
} | env \
  PATH="$tmpdir/bin:$PATH" \
  MOCK_DOCKER_LOG="$url_log" \
  "$url_project/install.sh" > "$tmpdir/external-url-output" 2>&1
grep -q "^DATABASE_URL='$database_url'$" "$url_project/.env"
grep -q "^MCP_ENABLED='false'$" "$url_project/.env"
if grep -q "$database_url" "$tmpdir/external-url-output" "$url_log"; then
  printf '%s\n' 'installer leaked pasted DATABASE_URL' >&2
  exit 1
fi

# 任意命令行参数都应被拒绝。
argument_project=$(make_project arguments)
if env PATH="$tmpdir/bin:$PATH" MOCK_DOCKER_LOG="$tmpdir/argument-docker.log" \
  "$argument_project/install.sh" --mode embedded >/dev/null 2>&1; then
  printf '%s\n' 'installer accepted command-line arguments' >&2
  exit 1
fi

# 用户在最终确认时取消，不应创建 .env。
cancel_project=$(make_project cancel)
if printf '\n\n\n\n\nn\n' | env \
  PATH="$tmpdir/bin:$PATH" MOCK_DOCKER_LOG="$tmpdir/cancel-docker.log" \
  "$cancel_project/install.sh" >/dev/null 2>&1; then
  printf '%s\n' 'installer returned success after cancellation' >&2
  exit 1
fi
[ ! -e "$cancel_project/.env" ] || {
  printf '%s\n' 'cancelled installer created .env' >&2
  exit 1
}

# 继续拒绝符号链接配置文件与重复的受管键。
symlink_project=$(make_project symlink)
printf '%s\n' sentinel > "$tmpdir/symlink-target.env"
ln -s "$tmpdir/symlink-target.env" "$symlink_project/.env"
if env PATH="$tmpdir/bin:$PATH" MOCK_DOCKER_LOG="$tmpdir/symlink-docker.log" \
  "$symlink_project/install.sh" >/dev/null 2>&1; then
  printf '%s\n' 'installer accepted a symlink env file' >&2
  exit 1
fi
grep -q '^sentinel$' "$tmpdir/symlink-target.env"

duplicate_project=$(make_project duplicate)
cp "$duplicate_project/.env.example" "$duplicate_project/.env"
printf '%s\n' 'ENCRYPTION_KEY=duplicate' >> "$duplicate_project/.env"
if env PATH="$tmpdir/bin:$PATH" MOCK_DOCKER_LOG="$tmpdir/duplicate-docker.log" \
  "$duplicate_project/install.sh" >/dev/null 2>&1; then
  printf '%s\n' 'installer accepted duplicate managed keys' >&2
  exit 1
fi

printf '%s\n' 'installer tests passed'

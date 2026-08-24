#!/bin/sh
set -eu

umask 077

launch_dir=$(pwd -P)
script_dir=$(
  CDPATH=''
  cd -- "$(dirname -- "$0")"
  pwd
)
project_dir=${ONE_SEARCH_PROJECT_DIR:-$script_dir}
env_file=${ONE_SEARCH_ENV_FILE:-$project_dir/.env}
case "$project_dir" in
  /*) ;;
  *) project_dir="$launch_dir/$project_dir" ;;
esac
case "$env_file" in
  /*) ;;
  *) env_file="$launch_dir/$env_file" ;;
esac
env_example="$project_dir/.env.example"

log() {
  printf '%s\n' "$*"
}

die() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
One Search 一键安装

用法：
  ./install.sh [选项]

选项：
  --mode embedded|external   数据库模式，首次安装默认 embedded
  --database-url-file FILE   从文件读取外部 DATABASE_URL，避免出现在命令行
  --database-network NAME    外部数据库所在的 Docker 网络，例如 shared-db
  --no-database-network      外部数据库不使用共享 Docker 网络
  --host-port PORT           Web 管理台端口，默认 5173
  --admin-username NAME      管理员用户名，默认 admin
  --admin-password PASSWORD  初始管理员密码；不指定则安全生成
  --postgres-password PASS   内置 PostgreSQL 密码；不指定则安全生成
  --encryption-key KEY       Key 加密密钥，至少 32 字符；不指定则安全生成
  --mcp-enabled true|false   是否启用 MCP，默认 true
  --no-build                 不主动执行 --build
  --timeout SECONDS          健康检查等待时间，默认 180 秒
  -y, --yes                  非交互模式；缺少外部 DATABASE_URL 时直接报错
  -h, --help                 显示帮助

外部数据库连接串优先从 --database-url-file 或环境变量 DATABASE_URL 读取；
未提供且处于交互终端时，
脚本会隐藏输入内容进行询问。建议不要把含密码的连接串写在命令行参数中。
EOF
}

get_env_value() {
  key="$1"
  if [ ! -f "$env_file" ]; then
    return
  fi
  raw=$(awk -v target="$key" '
    index($0, target "=") == 1 { value = substr($0, length(target) + 2) }
    END { if (value != "") print value }
  ' "$env_file")
  case "$raw" in
    \'*\')
      raw=${raw#\'}
      raw=${raw%\'}
      ;;
    \"*\")
      raw=${raw#\"}
      raw=${raw%\"}
      ;;
  esac
  printf '%s' "$raw"
}

validate_env_value() {
  key="$1"
  value="$2"
  case "$value" in
    *"'"*) die "$key 不能包含单引号；数据库连接串中请使用 %27 编码" ;;
  esac
  if [ "$(printf '%s' "$value" | tr -d '\r\n')" != "$value" ]; then
    die "$key 不能包含换行符"
  fi
}

write_env_line() {
  key="$1"
  value="$2"
  validate_env_value "$key" "$value"
  printf "%s='%s'\n" "$key" "$value" >> "$active_tmp_file"
}

write_configuration() {
  active_tmp_file=$(mktemp "${env_file}.tmp.XXXXXX")
  awk '
    BEGIN {
      split("ONE_SEARCH_INSTALL_MODE ONE_SEARCH_USE_SHARED_DB_NETWORK HOST_PORT POSTGRES_PASSWORD DATABASE_URL DATABASE_DOCKER_NETWORK ADMIN_USERNAME ADMIN_PASSWORD ENCRYPTION_KEY API_AUTH_REQUIRED MCP_ENABLED", items, " ")
      for (item_index in items) managed[items[item_index]] = 1
    }
    $0 == "# --- install.sh managed values ---" { next }
    {
      key = $0
      sub(/=.*/, "", key)
      if (!(key in managed)) print
    }
  ' "$env_file" > "$active_tmp_file"

  printf '\n%s\n' '# --- install.sh managed values ---' >> "$active_tmp_file"
  write_env_line ONE_SEARCH_INSTALL_MODE "$install_mode"
  write_env_line ONE_SEARCH_USE_SHARED_DB_NETWORK "$use_database_network"
  write_env_line HOST_PORT "$host_port"
  write_env_line POSTGRES_PASSWORD "$postgres_password"
  write_env_line DATABASE_URL "$database_url"
  write_env_line DATABASE_DOCKER_NETWORK "$database_network"
  write_env_line ADMIN_USERNAME "$admin_username"
  write_env_line ADMIN_PASSWORD "$admin_password"
  write_env_line ENCRYPTION_KEY "$encryption_key"
  write_env_line API_AUTH_REQUIRED "$api_auth_required"
  write_env_line MCP_ENABLED "$mcp_enabled"

  chmod 600 "$active_tmp_file"
  mv "$active_tmp_file" "$env_file"
  active_tmp_file=""
}

validate_env_file() {
  for key in \
    ONE_SEARCH_INSTALL_MODE ONE_SEARCH_USE_SHARED_DB_NETWORK HOST_PORT \
    POSTGRES_PASSWORD DATABASE_URL DATABASE_DOCKER_NETWORK ADMIN_USERNAME \
    ADMIN_PASSWORD ENCRYPTION_KEY API_AUTH_REQUIRED MCP_ENABLED
  do
    count=$(awk -v target="$key" 'index($0, target "=") == 1 { count++ } END { print count + 0 }' "$env_file")
    if [ "$count" -gt 1 ]; then
      die "$env_file 中存在重复的 $key，请先合并为一项"
    fi
  done
}

read_secret_file() {
  path="$1"
  [ -f "$path" ] && [ -r "$path" ] || die "无法读取 secret 文件：$path"
  value=$(tr -d '\r\n' < "$path")
  [ -n "$value" ] || die "secret 文件为空：$path"
  printf '%s' "$value"
}

generate_secret() {
  bytes="$1"
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$bytes"
    return
  fi
  if command -v od >/dev/null 2>&1 && [ -r /dev/urandom ]; then
    od -An -N "$bytes" -tx1 /dev/urandom | tr -d ' \n'
    return
  fi
  die "需要 openssl，或者可读取 /dev/urandom 的 od 来生成安全密钥"
}

normalize_bool() {
  value=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  case "$value" in
    true|1|yes|y|on) printf '%s' true ;;
    false|0|no|n|off) printf '%s' false ;;
    *) die "无效的布尔值：$1" ;;
  esac
}

prompt_secret() {
  prompt="$1"
  prompted_secret=""
  if [ ! -t 0 ]; then
    return
  fi
  printf '%s' "$prompt" >&2
  terminal_state=""
  if terminal_state=$(stty -g 2>/dev/null); then
    stty -echo 2>/dev/null || terminal_state=""
  fi
  IFS= read -r prompted_secret || true
  if [ -n "$terminal_state" ]; then
    stty "$terminal_state" 2>/dev/null || true
    terminal_state=""
    printf '\n' >&2
  fi
}

resolve_persistent_secret() {
  key="$1"
  supplied="$2"
  bytes="$3"
  existing=$(get_env_value "$key")
  if [ -n "$existing" ]; then
    if [ -n "$supplied" ] && [ "$supplied" != "$existing" ]; then
      die "$key 已存在，安装脚本不会自动替换持久化密钥或密码"
    fi
    printf '%s' "$existing"
    return
  fi
  if [ -n "$supplied" ]; then
    printf '%s' "$supplied"
    return
  fi
  generate_secret "$bytes"
}

run_compose() {
  if [ "$install_mode" = embedded ]; then
    docker compose --env-file "$env_file" -f "$project_dir/docker-compose.yml" "$@"
  elif [ "$use_database_network" = true ]; then
    docker compose --env-file "$env_file" \
      -f "$project_dir/docker-compose.external-db.yml" \
      -f "$project_dir/docker-compose.shared-db.yml" \
      "$@"
  else
    docker compose --env-file "$env_file" -f "$project_dir/docker-compose.external-db.yml" "$@"
  fi
}

check_prerequisites() {
  command -v docker >/dev/null 2>&1 || die "未找到 Docker，请先安装 Docker 24+"
  docker compose version >/dev/null 2>&1 || die "未找到 Docker Compose v2"
  docker info >/dev/null 2>&1 || die "无法连接 Docker daemon，请确认服务已启动且当前用户有权限"
}

wait_until_healthy() {
  attempt=0
  max_attempts=$health_timeout
  log "等待 One Search 健康检查通过……"
  while [ "$attempt" -lt "$max_attempts" ]; do
    if run_compose exec -T app curl --noproxy '*' -fsS http://127.0.0.1/healthz >/dev/null 2>&1; then
      return
    fi
    attempt=$((attempt + 1))
    sleep 1
  done
  run_compose ps >&2 || true
  run_compose logs --tail=80 app >&2 || true
  die "服务未能在 ${max_attempts} 秒内通过健康检查"
}

cleanup() {
  if [ -n "${terminal_state:-}" ]; then
    stty "$terminal_state" 2>/dev/null || true
    terminal_state=""
  fi
  if [ -n "${lock_dir:-}" ] && [ -d "$lock_dir" ]; then
    rmdir "$lock_dir" 2>/dev/null || true
  fi
  if [ -n "${active_tmp_file:-}" ] && [ -f "$active_tmp_file" ]; then
    rm -f "$active_tmp_file"
  fi
}

main() {
  mode_input=${ONE_SEARCH_INSTALL_MODE:-}
  database_url_input=${DATABASE_URL:-}
  database_url_file_input=${ONE_SEARCH_DATABASE_URL_FILE:-}
  database_network_input=${DATABASE_DOCKER_NETWORK:-}
  use_network_input=${ONE_SEARCH_USE_SHARED_DB_NETWORK:-}
  host_port_input=${HOST_PORT:-}
  admin_username_input=${ADMIN_USERNAME:-}
  admin_password_input=${ADMIN_PASSWORD:-}
  postgres_password_input=${POSTGRES_PASSWORD:-}
  encryption_key_input=${ENCRYPTION_KEY:-}
  mcp_enabled_input=${MCP_ENABLED:-}
  assume_yes=false
  no_build=false
  health_timeout=${ONE_SEARCH_INSTALL_HEALTH_TIMEOUT:-180}
  network_choice=""

  while [ "$#" -gt 0 ]; do
    case "$1" in
      --mode)
        [ "$#" -ge 2 ] || die "--mode 需要参数"
        mode_input="$2"
        shift 2
        ;;
      --mode=*) mode_input=${1#*=}; shift ;;
      --database-url-file)
        [ "$#" -ge 2 ] || die "--database-url-file 需要参数"
        database_url_file_input="$2"
        shift 2
        ;;
      --database-url-file=*) database_url_file_input=${1#*=}; shift ;;
      --database-network)
        [ "$#" -ge 2 ] || die "--database-network 需要参数"
        database_network_input="$2"
        network_choice=true
        shift 2
        ;;
      --database-network=*)
        database_network_input=${1#*=}
        network_choice=true
        shift
        ;;
      --no-database-network)
        network_choice=false
        shift
        ;;
      --host-port)
        [ "$#" -ge 2 ] || die "--host-port 需要参数"
        host_port_input="$2"
        shift 2
        ;;
      --host-port=*) host_port_input=${1#*=}; shift ;;
      --admin-username)
        [ "$#" -ge 2 ] || die "--admin-username 需要参数"
        admin_username_input="$2"
        shift 2
        ;;
      --admin-username=*) admin_username_input=${1#*=}; shift ;;
      --admin-password)
        [ "$#" -ge 2 ] || die "--admin-password 需要参数"
        admin_password_input="$2"
        shift 2
        ;;
      --admin-password=*) admin_password_input=${1#*=}; shift ;;
      --postgres-password)
        [ "$#" -ge 2 ] || die "--postgres-password 需要参数"
        postgres_password_input="$2"
        shift 2
        ;;
      --postgres-password=*) postgres_password_input=${1#*=}; shift ;;
      --encryption-key)
        [ "$#" -ge 2 ] || die "--encryption-key 需要参数"
        encryption_key_input="$2"
        shift 2
        ;;
      --encryption-key=*) encryption_key_input=${1#*=}; shift ;;
      --mcp-enabled)
        [ "$#" -ge 2 ] || die "--mcp-enabled 需要参数"
        mcp_enabled_input="$2"
        shift 2
        ;;
      --mcp-enabled=*) mcp_enabled_input=${1#*=}; shift ;;
      --timeout)
        [ "$#" -ge 2 ] || die "--timeout 需要参数"
        health_timeout="$2"
        shift 2
        ;;
      --timeout=*) health_timeout=${1#*=}; shift ;;
      --no-build) no_build=true; shift ;;
      -y|--yes) assume_yes=true; shift ;;
      -h|--help) usage; exit 0 ;;
      *) die "未知参数：$1（使用 --help 查看帮助）" ;;
    esac
  done

  if [ -n "$database_url_file_input" ]; then
    case "$database_url_file_input" in
      /*) ;;
      *) database_url_file_input="$launch_dir/$database_url_file_input" ;;
    esac
  fi

  cd "$project_dir"
  [ -f "$env_example" ] || die "找不到 $env_example"
  [ -f "$project_dir/docker-compose.yml" ] || die "当前目录不是 One Search 项目"
  case "$health_timeout" in
    ''|*[!0-9]*) die "--timeout 必须是正整数" ;;
  esac
  [ "$health_timeout" -ge 1 ] || die "--timeout 必须是正整数"

  if [ -L "$env_file" ]; then
    die "拒绝写入符号链接形式的配置文件：$env_file"
  fi
  if [ -e "$env_file" ] && [ ! -f "$env_file" ]; then
    die "配置路径不是普通文件：$env_file"
  fi

  lock_dir="$project_dir/.one-search-install.lock"
  if ! mkdir "$lock_dir" 2>/dev/null; then
    die "另一个安装进程可能正在运行；确认没有运行后删除 $lock_dir"
  fi
  trap cleanup EXIT
  trap 'cleanup; exit 130' INT TERM

  check_prerequisites

  if [ ! -f "$env_file" ]; then
    cp "$env_example" "$env_file"
  fi
  chmod 600 "$env_file"
  validate_env_file

  existing_mode=$(get_env_value ONE_SEARCH_INSTALL_MODE)
  install_mode=${mode_input:-${existing_mode:-embedded}}
  install_mode=$(printf '%s' "$install_mode" | tr '[:upper:]' '[:lower:]')
  case "$install_mode" in
    embedded|external) ;;
    *) die "--mode 必须是 embedded 或 external" ;;
  esac

  existing_host_port=$(get_env_value HOST_PORT)
  host_port=${host_port_input:-${existing_host_port:-5173}}
  case "$host_port" in
    ''|*[!0-9]*) die "HOST_PORT 必须是 1 到 65535 的整数" ;;
  esac
  [ "$host_port" -ge 1 ] && [ "$host_port" -le 65535 ] || die "HOST_PORT 必须是 1 到 65535 的整数"

  existing_admin_username=$(get_env_value ADMIN_USERNAME)
  admin_username=${admin_username_input:-${existing_admin_username:-admin}}
  [ -n "$admin_username" ] || die "ADMIN_USERNAME 不能为空"

  existing_api_auth=$(get_env_value API_AUTH_REQUIRED)
  api_auth_required=$(normalize_bool "${API_AUTH_REQUIRED:-${existing_api_auth:-true}}")
  existing_mcp_enabled=$(get_env_value MCP_ENABLED)
  mcp_enabled=$(normalize_bool "${mcp_enabled_input:-${existing_mcp_enabled:-true}}")

  existing_admin_password=$(get_env_value ADMIN_PASSWORD)
  generated_admin_password=false
  if [ -z "$existing_admin_password" ] && [ -z "$admin_password_input" ]; then
    generated_admin_password=true
  fi
  admin_password=$(resolve_persistent_secret ADMIN_PASSWORD "$admin_password_input" 16)
  [ "${#admin_password}" -ge 12 ] || die "ADMIN_PASSWORD 至少需要 12 个字符"

  encryption_key=$(resolve_persistent_secret ENCRYPTION_KEY "$encryption_key_input" 32)
  [ "${#encryption_key}" -ge 32 ] || die "ENCRYPTION_KEY 至少需要 32 个字符"

  postgres_password=""
  if [ "$install_mode" = embedded ]; then
    postgres_password=$(resolve_persistent_secret POSTGRES_PASSWORD "$postgres_password_input" 24)
    [ "${#postgres_password}" -ge 16 ] || die "POSTGRES_PASSWORD 至少需要 16 个字符"
  else
    postgres_password=$(get_env_value POSTGRES_PASSWORD)
  fi

  existing_database_url=$(get_env_value DATABASE_URL)
  if [ "$install_mode" = external ]; then
    if [ -n "$database_url_file_input" ]; then
      [ -z "$database_url_input" ] || die "DATABASE_URL 与 --database-url-file 不能同时使用"
      database_url_input=$(read_secret_file "$database_url_file_input")
    fi
    database_url=${database_url_input:-$existing_database_url}
    if [ -z "$database_url" ]; then
      if [ "$assume_yes" = false ]; then
        prompt_secret '请输入外部 PostgreSQL DATABASE_URL：'
        database_url=$prompted_secret
      fi
      [ -n "$database_url" ] || die "外部数据库模式必须提供 DATABASE_URL"
    fi
  else
    [ -z "$database_url_file_input" ] || die "--database-url-file 只适用于 external 模式"
    database_url=$existing_database_url
  fi

  existing_network_choice=$(get_env_value ONE_SEARCH_USE_SHARED_DB_NETWORK)
  if [ -n "$network_choice" ]; then
    use_database_network=$network_choice
  elif [ -n "$use_network_input" ]; then
    use_database_network=$(normalize_bool "$use_network_input")
  elif [ -n "$existing_network_choice" ]; then
    use_database_network=$(normalize_bool "$existing_network_choice")
  else
    use_database_network=false
  fi
  existing_database_network=$(get_env_value DATABASE_DOCKER_NETWORK)
  database_network=${database_network_input:-${existing_database_network:-shared-db}}
  if [ "$install_mode" = embedded ]; then
    if [ "$network_choice" = true ]; then
      die "--database-network 只适用于 external 模式"
    fi
    use_database_network=false
  fi
  if [ "$use_database_network" = true ]; then
    [ -n "$database_network" ] || die "DATABASE_DOCKER_NETWORK 不能为空"
    case "$database_network" in
      -*|*[!A-Za-z0-9_.-]*) die "DATABASE_DOCKER_NETWORK 只能包含字母、数字、点、下划线和连字符" ;;
    esac
  fi

  write_configuration

  if [ "$install_mode" = external ] && [ "$use_database_network" = true ]; then
    if ! docker network inspect "$database_network" >/dev/null 2>&1; then
      log "创建 Docker 网络：$database_network"
      docker network create --internal "$database_network" >/dev/null
    fi
  fi

  log "验证 Docker Compose 配置……"
  run_compose config --quiet

  if [ "$no_build" = true ]; then
    log "启动 One Search……"
    run_compose up -d
  else
    log "构建并启动 One Search……"
    run_compose up --build -d
  fi

  wait_until_healthy

  log ""
  log "One Search 安装完成"
  log "访问地址：http://localhost:$host_port"
  log "管理员账号：$admin_username"
  if [ "$generated_admin_password" = true ]; then
    log "初始管理员密码：$admin_password"
    log "该密码仅在目标数据库尚未创建此管理员时生效"
    log "请现在保存该密码；它同时保存在 $env_file"
  else
    log "管理员密码：沿用 $env_file 中的现有配置"
  fi
  log "数据库模式：$install_mode"
  if [ "$use_database_network" = true ]; then
    log "数据库网络：$database_network"
  fi
  log "配置文件：$env_file"
}

if [ "${ONE_SEARCH_INSTALLER_SKIP_MAIN:-false}" != true ]; then
  main "$@"
fi

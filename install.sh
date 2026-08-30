#!/bin/sh
set -eu

umask 077

script_dir=$(
  CDPATH=''
  cd -- "$(dirname -- "$0")"
  pwd
)
project_dir=$script_dir
env_file="$project_dir/.env"
env_example="$project_dir/.env.example"
health_timeout=180

# 安装配置只能通过向导和 .env 持久化。清除同名环境变量，避免调用者的
# shell 环境悄悄覆盖向导结果或 Docker Compose 的 --env-file。
unset \
  APP_ENV POSTGRES_DB POSTGRES_USER POSTGRES_PASSWORD DATABASE_URL \
  DATABASE_URL_FILE DATABASE_MODE DATABASE_DOCKER_NETWORK RUN_MIGRATIONS \
  ADMIN_USERNAME ADMIN_PASSWORD ENCRYPTION_KEY API_AUTH_REQUIRED MCP_ENABLED \
  MCP_PATH CORS_ALLOWED_ORIGINS HOST_PORT ONE_SEARCH_INSTALL_MODE \
  ONE_SEARCH_USE_SHARED_DB_NETWORK ONE_SEARCH_HTTP_PROXY \
  ONE_SEARCH_HTTPS_PROXY ONE_SEARCH_ALL_PROXY ONE_SEARCH_NO_PROXY \
  ONE_SEARCH_PROJECT_DIR ONE_SEARCH_ENV_FILE ONE_SEARCH_DATABASE_URL_FILE \
  ONE_SEARCH_INSTALL_HEALTH_TIMEOUT COMPOSE_FILE COMPOSE_PROJECT_NAME \
  COMPOSE_PROFILES 2>/dev/null || true

log() {
  printf '%s\n' "$*"
}

die() {
  printf '错误：%s\n' "$*" >&2
  exit 1
}

usage_error() {
  die "一键安装脚本无需参数，请直接运行 ./install.sh 并按提示选择"
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
    *"'"*) die "$key 不能包含单引号；数据库连接信息中的单引号请进行 URL 编码" ;;
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
  source_file=$env_example
  if [ -f "$env_file" ]; then
    source_file=$env_file
  fi
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
  ' "$source_file" > "$active_tmp_file"

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
  write_env_line API_AUTH_REQUIRED true
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
    *) return 1 ;;
  esac
}

prompt_line() {
  prompt_text="$1"
  default_value=${2:-}
  if [ -n "$default_value" ]; then
    printf '%s [%s]：' "$prompt_text" "$default_value" >&2
  else
    printf '%s：' "$prompt_text" >&2
  fi
  prompted_value=""
  IFS= read -r prompted_value || die "输入已结束，安装已取消"
  if [ -z "$prompted_value" ]; then
    prompted_value=$default_value
  fi
}

prompt_secret() {
  prompt_text="$1"
  printf '%s：' "$prompt_text" >&2
  prompted_secret=""
  terminal_state=""
  if [ -t 0 ] && terminal_state=$(stty -g 2>/dev/null); then
    stty -echo 2>/dev/null || terminal_state=""
  fi
  IFS= read -r prompted_secret || {
    if [ -n "$terminal_state" ]; then
      stty "$terminal_state" 2>/dev/null || true
      terminal_state=""
      printf '\n' >&2
    fi
    die "输入已结束，安装已取消"
  }
  if [ -n "$terminal_state" ]; then
    stty "$terminal_state" 2>/dev/null || true
    terminal_state=""
    printf '\n' >&2
  fi
}

prompt_confirmed_secret() {
  label="$1"
  min_length="$2"
  while :; do
    prompt_secret "请输入$label"
    first_secret=$prompted_secret
    [ "${#first_secret}" -ge "$min_length" ] || {
      log "$label 至少需要 $min_length 个字符，请重新输入"
      continue
    }
    validate_env_value "$label" "$first_secret"
    prompt_secret "请再次输入$label"
    if [ "$first_secret" = "$prompted_secret" ]; then
      prompted_secret=$first_secret
      return
    fi
    log "两次输入不一致，请重新输入"
  done
}

prompt_choice() {
  prompt_text="$1"
  default_choice="$2"
  allowed_choices="$3"
  while :; do
    prompt_line "$prompt_text" "$default_choice"
    case " $allowed_choices " in
      *" $prompted_value "*) return ;;
    esac
    log "请输入以下选项之一：$allowed_choices"
  done
}

prompt_yes_no() {
  prompt_text="$1"
  default_bool="$2"
  if [ "$default_bool" = true ]; then
    hint=Y/n
  else
    hint=y/N
  fi
  while :; do
    printf '%s [%s]：' "$prompt_text" "$hint" >&2
    answer=""
    IFS= read -r answer || die "输入已结束，安装已取消"
    if [ -z "$answer" ]; then
      prompted_bool=$default_bool
      return
    fi
    if prompted_bool=$(normalize_bool "$answer"); then
      return
    fi
    log "请输入 y 或 n"
  done
}

validate_port() {
  label="$1"
  value="$2"
  case "$value" in
    ''|*[!0-9]*) die "$label 必须是 1 到 65535 的整数" ;;
  esac
  [ "$value" -ge 1 ] && [ "$value" -le 65535 ] || die "$label 必须是 1 到 65535 的整数"
}

validate_network_name() {
  value="$1"
  [ -n "$value" ] || die "Docker 网络名称不能为空"
  case "$value" in
    -*|*[!A-Za-z0-9_.-]*) die "Docker 网络名称只能包含字母、数字、点、下划线和连字符" ;;
  esac
}

url_encode() {
  encoded_value=""
  for hex_byte in $(LC_ALL=C printf '%s' "$1" | od -An -tx1); do
    case "$hex_byte" in
      2d|2e|5f|7e|3[0-9]|4[1-9a-f]|5[0-9a]|6[1-9a-f]|7[0-9a])
        octal_byte=$(printf '%03o' "$((0x$hex_byte))")
        encoded_value="$encoded_value$(printf "\\$octal_byte")"
        ;;
      *)
        upper_hex=$(printf '%s' "$hex_byte" | tr '[:lower:]' '[:upper:]')
        encoded_value="$encoded_value%$upper_hex"
        ;;
    esac
  done
  printf '%s' "$encoded_value"
}

validate_database_url() {
  value="$1"
  [ -n "$value" ] || die "PostgreSQL 连接串不能为空"
  validate_env_value DATABASE_URL "$value"
  case "$value" in
    postgres://*|postgresql://*) ;;
    *) die "连接串必须以 postgres:// 或 postgresql:// 开头" ;;
  esac
}

configure_external_database() {
  default_network_choice=false
  existing_network_choice=$(get_env_value ONE_SEARCH_USE_SHARED_DB_NETWORK)
  if [ -n "$existing_network_choice" ]; then
    default_network_choice=$(normalize_bool "$existing_network_choice") || default_network_choice=false
  fi

  log ""
  log "外部数据库所在位置："
  log "  - 选择 y：PostgreSQL 在另一个 Docker Compose 项目的网络中"
  log "  - 选择 n：PostgreSQL 在远程服务器或当前宿主机上"
  prompt_yes_no "是否需要加入数据库所在的 Docker 网络" "$default_network_choice"
  use_database_network=$prompted_bool
  if [ "$use_database_network" = true ]; then
    existing_database_network=$(get_env_value DATABASE_DOCKER_NETWORK)
    prompt_line "Docker 网络名称" "${existing_database_network:-shared-db}"
    database_network=$prompted_value
    validate_network_name "$database_network"
    default_database_host=shared-postgres
  else
    database_network=shared-db
    default_database_host=host.docker.internal
  fi

  log ""
  log "外部 PostgreSQL 连接方式："
  log "  1) 按主机、端口、数据库、账号和密码逐项填写（推荐）"
  log "  2) 直接粘贴完整 DATABASE_URL"
  prompt_choice "请选择连接方式" 1 "1 2"
  connection_choice=$prompted_value

  if [ "$connection_choice" = 2 ]; then
    prompt_secret "请输入完整 PostgreSQL DATABASE_URL"
    database_url=$prompted_secret
    validate_database_url "$database_url"
  else
    prompt_line "数据库主机名" "$default_database_host"
    database_host=$prompted_value
    [ -n "$database_host" ] || die "数据库主机名不能为空"
    case "$database_host" in
      *[!A-Za-z0-9_.:-]*|*/*|*@*|*\?*|*\#*) die "数据库主机名包含不支持的字符" ;;
    esac

    prompt_line "数据库端口" 5432
    database_port=$prompted_value
    validate_port "数据库端口" "$database_port"

    prompt_line "数据库名称" one_search
    database_name=$prompted_value
    [ -n "$database_name" ] || die "数据库名称不能为空"

    prompt_line "数据库用户名" one_search
    database_user=$prompted_value
    [ -n "$database_user" ] || die "数据库用户名不能为空"

    prompt_confirmed_secret "数据库密码" 1
    database_password=$prompted_secret

    log ""
    log "SSL 模式："
    log "  1) disable：本机或受信任 Docker 网络"
    log "  2) require：加密连接，但不校验证书名称"
    log "  3) verify-full：校验证书和主机名"
    prompt_choice "请选择 SSL 模式" 1 "1 2 3"
    case "$prompted_value" in
      1) database_sslmode=disable ;;
      2) database_sslmode=require ;;
      3) database_sslmode=verify-full ;;
    esac

    database_host_url=$database_host
    case "$database_host_url" in
      *:*) database_host_url="[$database_host_url]" ;;
    esac
    database_url="postgresql://$(url_encode "$database_user"):$(url_encode "$database_password")@$database_host_url:$database_port/$(url_encode "$database_name")?sslmode=$database_sslmode"
  fi

}

configure_installation() {
  existing_mode=$(get_env_value ONE_SEARCH_INSTALL_MODE)
  if [ "$existing_mode" = external ]; then
    default_database_choice=2
  else
    default_database_choice=1
  fi

  log ""
  log "数据库模式："
  log "  1) 内置 PostgreSQL：数据保存在 Docker Volume 中（推荐）"
  log "  2) 外部 PostgreSQL：使用已有 PostgreSQL 15+"
  prompt_choice "请选择数据库模式" "$default_database_choice" "1 2"
  if [ "$prompted_value" = 1 ]; then
    install_mode=embedded
    use_database_network=false
    database_network=shared-db
    database_url=""
  else
    install_mode=external
    configure_external_database
  fi

  existing_host_port=$(get_env_value HOST_PORT)
  while :; do
    prompt_line "Web 管理台端口" "${existing_host_port:-5173}"
    host_port=$prompted_value
    case "$host_port" in
      ''|*[!0-9]*) log "端口必须是 1 到 65535 的整数"; continue ;;
    esac
    if [ "$host_port" -ge 1 ] && [ "$host_port" -le 65535 ]; then
      break
    fi
    log "端口必须是 1 到 65535 的整数"
  done

  existing_admin_username=$(get_env_value ADMIN_USERNAME)
  prompt_line "管理员用户名" "${existing_admin_username:-admin}"
  admin_username=$prompted_value
  [ -n "$admin_username" ] || die "管理员用户名不能为空"

  existing_admin_password=$(get_env_value ADMIN_PASSWORD)
  generated_admin_password=false
  if [ -n "$existing_admin_password" ]; then
    admin_password=$existing_admin_password
    log "管理员密码将沿用现有安全配置"
  else
    log ""
    log "管理员密码："
    log "  1) 自动生成安全密码（推荐）"
    log "  2) 自定义密码"
    prompt_choice "请选择管理员密码方式" 1 "1 2"
    if [ "$prompted_value" = 1 ]; then
      admin_password=$(generate_secret 16)
      generated_admin_password=true
    else
      prompt_confirmed_secret "管理员密码" 12
      admin_password=$prompted_secret
    fi
  fi
  [ "${#admin_password}" -ge 12 ] || die "现有 ADMIN_PASSWORD 少于 12 个字符，请移走 .env 后重新运行向导"

  existing_encryption_key=$(get_env_value ENCRYPTION_KEY)
  if [ -n "$existing_encryption_key" ]; then
    encryption_key=$existing_encryption_key
  else
    encryption_key=$(generate_secret 32)
  fi
  [ "${#encryption_key}" -ge 32 ] || die "现有 ENCRYPTION_KEY 少于 32 个字符，请先修复 .env"

  existing_postgres_password=$(get_env_value POSTGRES_PASSWORD)
  if [ "$install_mode" = embedded ]; then
    if [ -n "$existing_postgres_password" ]; then
      postgres_password=$existing_postgres_password
    else
      postgres_password=$(generate_secret 24)
    fi
    [ "${#postgres_password}" -ge 16 ] || die "现有 POSTGRES_PASSWORD 少于 16 个字符，请先修复 .env"
  else
    postgres_password=$existing_postgres_password
  fi

  existing_mcp_enabled=$(get_env_value MCP_ENABLED)
  default_mcp=true
  if [ -n "$existing_mcp_enabled" ]; then
    default_mcp=$(normalize_bool "$existing_mcp_enabled") || default_mcp=true
  fi
  prompt_yes_no "是否启用 MCP search/extract 工具" "$default_mcp"
  mcp_enabled=$prompted_bool
}

load_existing_configuration() {
  install_mode=$(get_env_value ONE_SEARCH_INSTALL_MODE)
  case "$install_mode" in
    embedded|external) ;;
    *) die "现有 .env 缺少有效的 ONE_SEARCH_INSTALL_MODE，请选择重新配置" ;;
  esac

  host_port=$(get_env_value HOST_PORT)
  validate_port HOST_PORT "$host_port"
  admin_username=$(get_env_value ADMIN_USERNAME)
  [ -n "$admin_username" ] || die "现有 ADMIN_USERNAME 不能为空"
  admin_password=$(get_env_value ADMIN_PASSWORD)
  [ "${#admin_password}" -ge 12 ] || die "现有 ADMIN_PASSWORD 至少需要 12 个字符"
  encryption_key=$(get_env_value ENCRYPTION_KEY)
  [ "${#encryption_key}" -ge 32 ] || die "现有 ENCRYPTION_KEY 至少需要 32 个字符"
  postgres_password=$(get_env_value POSTGRES_PASSWORD)
  database_url=$(get_env_value DATABASE_URL)
  database_network=$(get_env_value DATABASE_DOCKER_NETWORK)
  database_network=${database_network:-shared-db}
  existing_network_choice=$(get_env_value ONE_SEARCH_USE_SHARED_DB_NETWORK)
  use_database_network=$(normalize_bool "${existing_network_choice:-false}") || die "现有 ONE_SEARCH_USE_SHARED_DB_NETWORK 无效"
  mcp_enabled=$(normalize_bool "$(get_env_value MCP_ENABLED)") || die "现有 MCP_ENABLED 无效"
  generated_admin_password=false

  if [ "$install_mode" = embedded ]; then
    [ "${#postgres_password}" -ge 16 ] || die "现有 POSTGRES_PASSWORD 至少需要 16 个字符"
    use_database_network=false
  else
    validate_database_url "$database_url"
    if [ "$use_database_network" = true ]; then
      validate_network_name "$database_network"
    fi
  fi
}

show_summary() {
  log ""
  log "安装配置摘要"
  if [ "$install_mode" = embedded ]; then
    log "  数据库：内置 PostgreSQL"
  else
    log "  数据库：外部 PostgreSQL"
    log "  连接信息：已安全保存，摘要中不显示"
    if [ "$use_database_network" = true ]; then
      log "  Docker 网络：$database_network"
    else
      log "  Docker 网络：不额外加入共享网络"
    fi
  fi
  log "  管理台：http://localhost:$host_port"
  log "  管理员：$admin_username"
  log "  API Token 认证：启用"
  log "  MCP：$mcp_enabled"
  log "  配置文件：$env_file（权限 600）"
  log ""
  prompt_yes_no "确认以上配置并开始安装" true
  [ "$prompted_bool" = true ] || die "安装已取消，未修改配置"
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
  log "等待 One Search 健康检查通过……"
  while [ "$attempt" -lt "$health_timeout" ]; do
    if run_compose exec -T app curl --noproxy '*' -fsS http://127.0.0.1/healthz >/dev/null 2>&1; then
      return
    fi
    attempt=$((attempt + 1))
    sleep 1
  done
  run_compose ps >&2 || true
  run_compose logs --tail=80 app >&2 || true
  die "服务未能在 ${health_timeout} 秒内通过健康检查"
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
  [ "$#" -eq 0 ] || usage_error
  cd "$project_dir"
  [ -f "$env_example" ] || die "找不到 $env_example"
  [ -f "$project_dir/docker-compose.yml" ] || die "当前目录不是 One Search 项目"
  [ -f "$project_dir/docker-compose.external-db.yml" ] || die "缺少外部数据库 Compose 配置"

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
  if [ -f "$env_file" ]; then
    chmod 600 "$env_file"
    validate_env_file
  fi

  log ""
  log "========================================"
  log "          One Search 安装向导"
  log "========================================"
  log "无需设置环境变量，也无需传入参数。"

  reuse_existing=false
  existing_mode=$(get_env_value ONE_SEARCH_INSTALL_MODE)
  if [ "$existing_mode" = embedded ] || [ "$existing_mode" = external ]; then
    log ""
    log "检测到现有安装配置：$existing_mode"
    log "  1) 沿用现有配置启动或更新（推荐）"
    log "  2) 重新运行配置向导"
    prompt_choice "请选择操作" 1 "1 2"
    if [ "$prompted_value" = 1 ]; then
      reuse_existing=true
      load_existing_configuration
    fi
  fi

  if [ "$reuse_existing" = false ]; then
    configure_installation
  fi

  show_summary
  if [ "$reuse_existing" = false ]; then
    write_configuration
  fi

  if [ "$install_mode" = external ] && [ "$use_database_network" = true ]; then
    if ! docker network inspect "$database_network" >/dev/null 2>&1; then
      log "创建 Docker 网络：$database_network"
      docker network create --internal "$database_network" >/dev/null
      log "请确保 PostgreSQL 容器也已加入该网络，并可通过连接串中的主机名访问"
    fi
  fi

  log "验证 Docker Compose 配置……"
  run_compose config --quiet
  log "构建并启动 One Search……"
  run_compose up --build -d
  wait_until_healthy

  log ""
  log "One Search 安装完成"
  log "访问地址：http://localhost:$host_port"
  log "管理员账号：$admin_username"
  if [ "$generated_admin_password" = true ]; then
    log "初始管理员密码：$admin_password"
    log "请现在保存该密码；它也保存在权限为 600 的 $env_file"
  else
    log "管理员密码：使用向导中输入或现有配置中的密码"
  fi
  if [ "$install_mode" = embedded ]; then
    log "数据库模式：内置 PostgreSQL"
  else
    log "数据库模式：外部 PostgreSQL"
  fi
}

main "$@"

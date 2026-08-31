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
  MCP_PATH CORS_ALLOWED_ORIGINS HOST_PORT TZ SEARCHMELD_INSTALL_MODE \
  SEARCHMELD_USE_SHARED_DB_NETWORK SEARCHMELD_USE_CUSTOM_DNS \
  SEARCHMELD_DNS_PRIMARY SEARCHMELD_DNS_SECONDARY SEARCHMELD_HTTP_PROXY \
  SEARCHMELD_HTTPS_PROXY SEARCHMELD_ALL_PROXY SEARCHMELD_NO_PROXY \
  SEARCHMELD_PROJECT_DIR SEARCHMELD_ENV_FILE SEARCHMELD_DATABASE_URL_FILE \
  SEARCHMELD_INSTALL_HEALTH_TIMEOUT ONE_SEARCH_INSTALL_MODE \
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

get_compat_env_value() {
  primary_key="$1"
  legacy_key="$2"
  primary_value=$(get_env_value "$primary_key")
  if [ -n "$primary_value" ]; then
    printf '%s' "$primary_value"
    return
  fi
  get_env_value "$legacy_key"
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
      split("SEARCHMELD_INSTALL_MODE SEARCHMELD_USE_SHARED_DB_NETWORK SEARCHMELD_USE_CUSTOM_DNS SEARCHMELD_DNS_PRIMARY SEARCHMELD_DNS_SECONDARY ONE_SEARCH_INSTALL_MODE ONE_SEARCH_USE_SHARED_DB_NETWORK HOST_PORT POSTGRES_PASSWORD DATABASE_URL DATABASE_DOCKER_NETWORK ADMIN_USERNAME ADMIN_PASSWORD ENCRYPTION_KEY API_AUTH_REQUIRED MCP_ENABLED", items, " ")
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
  write_env_line SEARCHMELD_INSTALL_MODE "$install_mode"
  write_env_line SEARCHMELD_USE_SHARED_DB_NETWORK "$use_database_network"
  write_env_line SEARCHMELD_USE_CUSTOM_DNS "$use_custom_dns"
  write_env_line SEARCHMELD_DNS_PRIMARY "$dns_primary"
  write_env_line SEARCHMELD_DNS_SECONDARY "$dns_secondary"
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
    SEARCHMELD_INSTALL_MODE SEARCHMELD_USE_SHARED_DB_NETWORK \
    SEARCHMELD_USE_CUSTOM_DNS SEARCHMELD_DNS_PRIMARY SEARCHMELD_DNS_SECONDARY \
    ONE_SEARCH_INSTALL_MODE ONE_SEARCH_USE_SHARED_DB_NETWORK HOST_PORT \
    POSTGRES_PASSWORD DATABASE_URL DATABASE_DOCKER_NETWORK ADMIN_USERNAME \
    ADMIN_PASSWORD ENCRYPTION_KEY API_AUTH_REQUIRED MCP_ENABLED
  do
    count=$(awk -v target="$key" 'index($0, target "=") == 1 { count++ } END { print count + 0 }' "$env_file")
    if [ "$count" -gt 1 ]; then
      die "$env_file 中存在重复的 $key，请先合并为一项"
    fi
  done

  primary_mode=$(get_env_value SEARCHMELD_INSTALL_MODE)
  legacy_mode=$(get_env_value ONE_SEARCH_INSTALL_MODE)
  if [ -n "$primary_mode" ] && [ -n "$legacy_mode" ] && [ "$primary_mode" != "$legacy_mode" ]; then
    die "$env_file 中 SEARCHMELD_INSTALL_MODE 与旧版 ONE_SEARCH_INSTALL_MODE 冲突"
  fi
  primary_network=$(get_env_value SEARCHMELD_USE_SHARED_DB_NETWORK)
  legacy_network=$(get_env_value ONE_SEARCH_USE_SHARED_DB_NETWORK)
  if [ -n "$primary_network" ] && [ -n "$legacy_network" ] && [ "$primary_network" != "$legacy_network" ]; then
    die "$env_file 中 SEARCHMELD_USE_SHARED_DB_NETWORK 与旧版 ONE_SEARCH_USE_SHARED_DB_NETWORK 冲突"
  fi
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
  if [ "$value" -lt 1 ] || [ "$value" -gt 65535 ]; then
    die "$label 必须是 1 到 65535 的整数"
  fi
}

validate_network_name() {
  value="$1"
  [ -n "$value" ] || die "Docker 网络名称不能为空"
  case "$value" in
    -*|*[!A-Za-z0-9_.-]*) die "Docker 网络名称只能包含字母、数字、点、下划线和连字符" ;;
  esac
}

is_ipv4_literal() {
  printf '%s\n' "$1" | awk -F. '
    NF != 4 { exit 1 }
    {
      for (field_index = 1; field_index <= 4; field_index++) {
        if ($field_index !~ /^[0-9]+$/ || $field_index < 0 || $field_index > 255) exit 1
        if (length($field_index) > 1 && substr($field_index, 1, 1) == "0") exit 1
      }
    }
  '
}

is_ipv6_literal() {
  printf '%s\n' "$1" | awk '
    function valid_group(group) {
      return length(group) >= 1 && length(group) <= 4 && group ~ /^[0-9A-Fa-f]+$/
    }
    function count_groups(part, groups, count, group_index) {
      if (part == "") return 0
      count = split(part, groups, ":")
      for (group_index = 1; group_index <= count; group_index++) {
        if (!valid_group(groups[group_index])) return -1
      }
      return count
    }
    function valid_ipv4(value, octets, count, octet_index) {
      count = split(value, octets, ".")
      if (count != 4) return 0
      for (octet_index = 1; octet_index <= count; octet_index++) {
        if (octets[octet_index] !~ /^[0-9]+$/ || octets[octet_index] < 0 || octets[octet_index] > 255) return 0
        if (length(octets[octet_index]) > 1 && substr(octets[octet_index], 1, 1) == "0") return 0
      }
      return 1
    }
    {
      value = $0
      if (value == "" || value !~ /^[0-9A-Fa-f:.]+$/) exit 1
      if (index(value, ".") > 0) {
        last_colon = 0
        for (character_index = 1; character_index <= length(value); character_index++) {
          if (substr(value, character_index, 1) == ":") last_colon = character_index
        }
        if (last_colon == 0) exit 1
        ipv4_suffix = substr(value, last_colon + 1)
        if (!valid_ipv4(ipv4_suffix)) exit 1
        value = substr(value, 1, last_colon) "0:0"
      }
      if (value ~ /:::/) exit 1
      compressed_at = index(value, "::")
      if (compressed_at > 0) {
        left = substr(value, 1, compressed_at - 1)
        right = substr(value, compressed_at + 2)
        if (index(right, "::") > 0) exit 1
        left_count = count_groups(left)
        right_count = count_groups(right)
        if (left_count < 0 || right_count < 0 || left_count + right_count >= 8) exit 1
        exit 0
      }
      if (substr(value, 1, 1) == ":" || substr(value, length(value), 1) == ":") exit 1
      if (count_groups(value) != 8) exit 1
    }
  '
}

is_dns_ip_literal() {
  value="$1"
  case "$value" in
    *:*) is_ipv6_literal "$value" ;;
    *) is_ipv4_literal "$value" ;;
  esac
}

canonicalize_dns_ip_literal() {
  canonical_dns_value="$1"
  case "$canonical_dns_value" in
    *:*)
      printf '%s\n' "$canonical_dns_value" | awk '
        function hex_to_decimal(group, result, character_index, digit) {
          group = tolower(group)
          result = 0
          for (character_index = 1; character_index <= length(group); character_index++) {
            digit = index("0123456789abcdef", substr(group, character_index, 1)) - 1
            result = result * 16 + digit
          }
          return result
        }
        function append_group(group_value) {
          if (canonical != "") canonical = canonical ":"
          canonical = canonical sprintf("%x", group_value)
        }
        {
          value = $0
          if (index(value, ".") > 0) {
            last_colon = 0
            for (character_index = 1; character_index <= length(value); character_index++) {
              if (substr(value, character_index, 1) == ":") last_colon = character_index
            }
            ipv4_suffix = substr(value, last_colon + 1)
            split(ipv4_suffix, octets, ".")
            first_ipv4_group = octets[1] * 256 + octets[2]
            second_ipv4_group = octets[3] * 256 + octets[4]
            value = substr(value, 1, last_colon) sprintf("%x:%x", first_ipv4_group, second_ipv4_group)
          }

          compressed_at = index(value, "::")
          if (compressed_at > 0) {
            left = substr(value, 1, compressed_at - 1)
            right = substr(value, compressed_at + 2)
            left_count = left == "" ? 0 : split(left, left_groups, ":")
            right_count = right == "" ? 0 : split(right, right_groups, ":")
            for (group_index = 1; group_index <= left_count; group_index++) {
              append_group(hex_to_decimal(left_groups[group_index]))
            }
            for (group_index = 1; group_index <= 8 - left_count - right_count; group_index++) {
              append_group(0)
            }
            for (group_index = 1; group_index <= right_count; group_index++) {
              append_group(hex_to_decimal(right_groups[group_index]))
            }
          } else {
            group_count = split(value, groups, ":")
            for (group_index = 1; group_index <= group_count; group_index++) {
              append_group(hex_to_decimal(groups[group_index]))
            }
          }
          print canonical
        }
      '
      ;;
    *)
      printf '%s\n' "$canonical_dns_value" | awk -F. '{
        printf "%d.%d.%d.%d\n", $1, $2, $3, $4
      }'
      ;;
  esac
}

dns_ip_literals_equal() {
  first_dns_canonical=$(canonicalize_dns_ip_literal "$1")
  second_dns_canonical=$(canonicalize_dns_ip_literal "$2")
  [ "$first_dns_canonical" = "$second_dns_canonical" ]
}

validate_custom_dns_configuration() {
  is_dns_ip_literal "$dns_primary" || die "首选 DNS 必须是有效的 IPv4 或 IPv6 地址"
  is_dns_ip_literal "$dns_secondary" || die "备用 DNS 必须是有效的 IPv4 或 IPv6 地址"
  if dns_ip_literals_equal "$dns_primary" "$dns_secondary"; then
    die "首选 DNS 和备用 DNS 不能相同"
  fi
}

configure_custom_dns() {
  existing_custom_dns=$(get_env_value SEARCHMELD_USE_CUSTOM_DNS)
  default_custom_dns=$(normalize_bool "${existing_custom_dns:-false}") || default_custom_dns=false
  existing_dns_primary=$(get_env_value SEARCHMELD_DNS_PRIMARY)
  existing_dns_secondary=$(get_env_value SEARCHMELD_DNS_SECONDARY)

  log ""
  log "容器 DNS："
  log "  - 默认使用 Docker/宿主机 DNS"
  log "  - Mihomo Fake-IP、企业内网或域名分流环境通常应保持默认"
  log "  - 仅在 Docker DNS 间歇解析失败时启用自定义 DNS"
  prompt_yes_no "是否为 SearchMeld 容器配置两台自定义 DNS" "$default_custom_dns"
  use_custom_dns=$prompted_bool

  if [ "$use_custom_dns" = false ]; then
    dns_primary=""
    dns_secondary=""
    return
  fi

  while :; do
    prompt_line "首选 DNS IP" "$existing_dns_primary"
    dns_primary=$prompted_value
    if is_dns_ip_literal "$dns_primary"; then
      break
    fi
    log "首选 DNS 必须是有效的 IPv4 或 IPv6 地址"
  done

  while :; do
    prompt_line "备用 DNS IP" "$existing_dns_secondary"
    dns_secondary=$prompted_value
    if ! is_dns_ip_literal "$dns_secondary"; then
      log "备用 DNS 必须是有效的 IPv4 或 IPv6 地址"
      continue
    fi
    if dns_ip_literals_equal "$dns_primary" "$dns_secondary"; then
      log "备用 DNS 不能与首选 DNS 相同"
      continue
    fi
    break
  done
}

url_encode() {
  encoded_value=""
  for hex_byte in $(LC_ALL=C printf '%s' "$1" | od -An -tx1); do
    case "$hex_byte" in
      2d|2e|5f|7e|3[0-9]|4[1-9a-f]|5[0-9a]|6[1-9a-f]|7[0-9a])
        octal_byte=$(printf '%03o' "$((0x$hex_byte))")
        encoded_value="$encoded_value$(printf '%b' "\\$octal_byte")"
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
  existing_network_choice=$(get_compat_env_value SEARCHMELD_USE_SHARED_DB_NETWORK ONE_SEARCH_USE_SHARED_DB_NETWORK)
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
  existing_mode=$(get_compat_env_value SEARCHMELD_INSTALL_MODE ONE_SEARCH_INSTALL_MODE)
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

  configure_custom_dns
}

load_existing_configuration() {
  install_mode=$(get_compat_env_value SEARCHMELD_INSTALL_MODE ONE_SEARCH_INSTALL_MODE)
  case "$install_mode" in
    embedded|external) ;;
    *) die "现有 .env 缺少有效的 SEARCHMELD_INSTALL_MODE（或旧版 ONE_SEARCH_INSTALL_MODE），请选择重新配置" ;;
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
  existing_network_choice=$(get_compat_env_value SEARCHMELD_USE_SHARED_DB_NETWORK ONE_SEARCH_USE_SHARED_DB_NETWORK)
  use_database_network=$(normalize_bool "${existing_network_choice:-false}") || die "现有 SEARCHMELD_USE_SHARED_DB_NETWORK（或旧版 ONE_SEARCH_USE_SHARED_DB_NETWORK）无效"
  mcp_enabled=$(normalize_bool "$(get_env_value MCP_ENABLED)") || die "现有 MCP_ENABLED 无效"
  existing_custom_dns=$(get_env_value SEARCHMELD_USE_CUSTOM_DNS)
  use_custom_dns=$(normalize_bool "${existing_custom_dns:-false}") || die "现有 SEARCHMELD_USE_CUSTOM_DNS 无效"
  dns_primary=$(get_env_value SEARCHMELD_DNS_PRIMARY)
  dns_secondary=$(get_env_value SEARCHMELD_DNS_SECONDARY)
  if [ "$use_custom_dns" = true ]; then
    validate_custom_dns_configuration
  else
    dns_primary=""
    dns_secondary=""
  fi
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
  log "执行摘要"
  case "$install_action" in
    update) log "  操作：从 $update_upstream 更新并重新构建" ;;
    rebuild) log "  操作：使用当前版本重新构建并启动" ;;
    reconfigure) log "  操作：重新配置并构建" ;;
    *) log "  操作：首次安装" ;;
  esac
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
  if [ "$use_custom_dns" = true ]; then
    log "  自定义 DNS：启用（$dns_primary、$dns_secondary）"
  else
    log "  自定义 DNS：关闭（使用 Docker/宿主机 DNS）"
  fi
  configured_timezone=$(get_env_value TZ)
  configured_timezone=${configured_timezone:-Asia/Shanghai}
  log "  容器时区：$configured_timezone"
  log "  配置文件：$env_file（权限 600）"
  log ""
  prompt_yes_no "确认以上配置并开始安装" true
  [ "$prompted_bool" = true ] || die "安装已取消，未修改配置"
}

source_update_available() {
  command -v git >/dev/null 2>&1 || return 1
  git -C "$project_dir" rev-parse --is-inside-work-tree >/dev/null 2>&1 || return 1
  git_root=$(git -C "$project_dir" rev-parse --show-toplevel 2>/dev/null) || return 1
  [ "$git_root" = "$project_dir" ] || return 1
  git -C "$project_dir" symbolic-ref --quiet --short HEAD >/dev/null 2>&1 || return 1
  update_check_upstream=$(git -C "$project_dir" rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null) || return 1
  case "$update_check_upstream" in
    */*) update_check_remote=${update_check_upstream%%/*} ;;
    *) return 1 ;;
  esac
  git -C "$project_dir" remote get-url "$update_check_remote" >/dev/null 2>&1
}

prepare_source_update() {
  source_update_available || die "当前安装目录不是可更新的 Git 仓库"
  current_branch=$(git -C "$project_dir" symbolic-ref --quiet --short HEAD 2>/dev/null) || \
    die "当前 Git 仓库处于 detached HEAD，无法确定要更新的分支"
  update_upstream=$(git -C "$project_dir" rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null) || \
    die "当前分支 $current_branch 没有配置上游分支，请先设置 Git upstream"
  case "$update_upstream" in
    */*) ;;
    *) die "无法从上游分支 $update_upstream 确定 Git remote" ;;
  esac
  update_remote=${update_upstream%%/*}
  git -C "$project_dir" remote get-url "$update_remote" >/dev/null 2>&1 || \
    die "找不到 Git remote：$update_remote"

  tracked_changes=$(git -C "$project_dir" status --porcelain --untracked-files=no) || \
    die "无法检查 Git 工作区状态"
  [ -z "$tracked_changes" ] || \
    die "检测到未提交的已跟踪文件修改。为避免覆盖本地改动，请先提交、暂存到其它位置或还原后再更新"
  previous_revision=$(git -C "$project_dir" rev-parse HEAD) || die "无法读取当前 Git 版本"
  updated_revision=$previous_revision
}

perform_source_update() {
  log "从 $update_remote 获取远端更新……"
  git -C "$project_dir" fetch --prune "$update_remote"
  target_revision=$(git -C "$project_dir" rev-parse "$update_upstream") || \
    die "无法读取远端版本 $update_upstream"
  if [ "$target_revision" = "$previous_revision" ]; then
    log "当前代码已经是 $update_upstream 的最新版本"
    return
  fi
  if ! git -C "$project_dir" merge-base --is-ancestor "$previous_revision" "$target_revision"; then
    die "本地分支与 $update_upstream 已分叉，自动更新只支持 fast-forward；请手动处理 Git 历史"
  fi

  update_count=$(git -C "$project_dir" rev-list --count "$previous_revision..$target_revision") || \
    die "无法计算待更新提交数量"
  log "发现 $update_count 个新提交，正在快进更新……"
  git -C "$project_dir" merge --ff-only "$update_upstream"
  updated_revision=$(git -C "$project_dir" rev-parse HEAD) || die "无法确认更新后的 Git 版本"

  for required_file in \
    install.sh .env.example docker-compose.yml docker-compose.external-db.yml \
    docker-compose.shared-db.yml docker-compose.dns.yml
  do
    [ -f "$project_dir/$required_file" ] || die "更新后的项目缺少 $required_file"
  done
  previous_short=$(printf '%s' "$previous_revision" | cut -c1-12)
  updated_short=$(printf '%s' "$updated_revision" | cut -c1-12)
  log "代码已更新：$previous_short -> $updated_short"
}

run_compose() {
  if [ "$install_mode" = embedded ]; then
    if [ "$use_custom_dns" = true ]; then
      docker compose --env-file "$env_file" \
        -f "$project_dir/docker-compose.yml" \
        -f "$project_dir/docker-compose.dns.yml" \
        "$@"
    else
      docker compose --env-file "$env_file" -f "$project_dir/docker-compose.yml" "$@"
    fi
  elif [ "$use_database_network" = true ]; then
    if [ "$use_custom_dns" = true ]; then
      docker compose --env-file "$env_file" \
        -f "$project_dir/docker-compose.external-db.yml" \
        -f "$project_dir/docker-compose.shared-db.yml" \
        -f "$project_dir/docker-compose.dns.yml" \
        "$@"
    else
      docker compose --env-file "$env_file" \
        -f "$project_dir/docker-compose.external-db.yml" \
        -f "$project_dir/docker-compose.shared-db.yml" \
        "$@"
    fi
  else
    if [ "$use_custom_dns" = true ]; then
      docker compose --env-file "$env_file" \
        -f "$project_dir/docker-compose.external-db.yml" \
        -f "$project_dir/docker-compose.dns.yml" \
        "$@"
    else
      docker compose --env-file "$env_file" -f "$project_dir/docker-compose.external-db.yml" "$@"
    fi
  fi
}

check_prerequisites() {
  command -v docker >/dev/null 2>&1 || die "未找到 Docker，请先安装 Docker 24+"
  docker compose version >/dev/null 2>&1 || die "未找到 Docker Compose v2"
  docker info >/dev/null 2>&1 || die "无法连接 Docker daemon，请确认服务已启动且当前用户有权限"
}

wait_until_healthy() {
  attempt=0
  log "等待 SearchMeld 健康检查通过……"
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
  [ -f "$project_dir/docker-compose.yml" ] || die "当前目录不是 SearchMeld 项目"
  [ -f "$project_dir/docker-compose.external-db.yml" ] || die "缺少外部数据库 Compose 配置"
  [ -f "$project_dir/docker-compose.shared-db.yml" ] || die "缺少共享数据库网络 Compose 配置"
  [ -f "$project_dir/docker-compose.dns.yml" ] || die "缺少自定义 DNS Compose 配置"

  if [ -L "$env_file" ]; then
    die "拒绝写入符号链接形式的配置文件：$env_file"
  fi
  if [ -e "$env_file" ] && [ ! -f "$env_file" ]; then
    die "配置路径不是普通文件：$env_file"
  fi

  legacy_lock_dir="$project_dir/.one-search-install.lock"
  [ ! -d "$legacy_lock_dir" ] || die "检测到旧版安装锁；确认没有安装进程后删除 $legacy_lock_dir"
  lock_dir="$project_dir/.searchmeld-install.lock"
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
  log "          SearchMeld 安装向导"
  log "========================================"
  log "无需设置环境变量，也无需传入参数。"

  reuse_existing=false
  install_action=install
  previous_revision=""
  updated_revision=""
  update_upstream=""
  update_remote=""
  existing_mode=$(get_compat_env_value SEARCHMELD_INSTALL_MODE ONE_SEARCH_INSTALL_MODE)
  if [ "$existing_mode" = embedded ] || [ "$existing_mode" = external ]; then
    log ""
    log "检测到现有安装配置：$existing_mode"
    if source_update_available; then
      log "  1) 从远端更新到最新版本并重新构建（推荐）"
      log "  2) 使用当前版本重新构建并启动"
      log "  3) 重新运行配置向导"
      prompt_choice "请选择操作" 1 "1 2 3"
      case "$prompted_value" in
        1)
          reuse_existing=true
          install_action=update
          load_existing_configuration
          prepare_source_update
          ;;
        2)
          reuse_existing=true
          install_action=rebuild
          load_existing_configuration
          ;;
        3)
          install_action=reconfigure
          ;;
      esac
    else
      log "未检测到带上游分支的 Git 安装目录，无法自动拉取新版本。"
      log "  1) 使用当前版本重新构建并启动（推荐）"
      log "  2) 重新运行配置向导"
      prompt_choice "请选择操作" 1 "1 2"
      if [ "$prompted_value" = 1 ]; then
        reuse_existing=true
        install_action=rebuild
        load_existing_configuration
      else
        install_action=reconfigure
      fi
    fi
  fi

  if [ "$reuse_existing" = false ]; then
    configure_installation
  fi

  show_summary
  if [ "$reuse_existing" = false ]; then
    write_configuration
  fi

  if [ "$install_action" = update ]; then
    perform_source_update
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
  if [ "$install_action" = update ]; then
    log "拉取基础镜像并构建新版本（旧服务会继续运行到构建完成）……"
    run_compose build --pull
    log "切换到新版本……"
    run_compose up -d --remove-orphans
  else
    log "构建并启动 SearchMeld……"
    run_compose up --build -d --remove-orphans
  fi
  wait_until_healthy

  log ""
  log "SearchMeld 安装完成"
  if [ "$install_action" = update ]; then
    updated_short=$(printf '%s' "$updated_revision" | cut -c1-12)
    log "当前版本：$updated_short（$update_upstream）"
  fi
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

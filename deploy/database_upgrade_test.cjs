'use strict'

if (process.env.GITHUB_ACTIONS !== 'true') {
  throw new Error('Database upgrade checks may run only in GitHub Actions')
}

const { execFile } = require('node:child_process')
const { randomBytes } = require('node:crypto')
const { mkdir, rm, writeFile } = require('node:fs/promises')
const http = require('node:http')
const { join, resolve } = require('node:path')
const { isDeepStrictEqual, promisify } = require('node:util')

const execute = promisify(execFile)
const baselineRevision = '1e10c565dc5443e5bafcc391a95563df51c08bad'
const currentImage = process.env.UPGRADE_TEST_IMAGE || 'searchmeld:ci-all-in-one'
const incompatibleImage = 'postgres:17.11-alpine3.23'
const prefix = process.env.UPGRADE_CONTAINER_PREFIX
const proof = { 'X-SearchMeld-Admin': '1' }
const cookieName = 'searchmeld_admin_session'
const database = 'upgrade_fixture'
const containers = new Set()
const volumes = new Set()
const controller = new AbortController()
let temporary
let ownsTemporary = false
let networkCreated = false
let builderCreated = false
let baselineBuilt = false
let ownsIncompatibleImage = false
let helperNumber = 0
let forwarder
let upstream
let origin
let closing

class FixtureError extends Error {}

function check(condition, label) {
  // Assertions must never render actual/expected credentials or database rows.
  if (!condition) throw new FixtureError(label)
}

function decodeJSON(value, label) {
  try { return JSON.parse(value) } catch { throw new FixtureError(`${label}: invalid JSON`) }
}

async function command(file, args, { timeout = 90000, cleanup = false, input } = {}) {
  try {
    const execution = execute(file, args, {
      timeout, killSignal: 'SIGKILL', maxBuffer: 8 * 1024 * 1024,
      signal: cleanup ? undefined : controller.signal
    })
    execution.child.stdin.on('error', () => {})
    execution.child.stdin.end(input)
    const { stdout } = await execution
    return stdout.trim()
  } catch (error) {
    const code = Number.isInteger(error.code) ? error.code : 'unavailable-or-timeout'
    // execFile errors contain command arguments and stdout/stderr. Never print them.
    throw new FixtureError(`${file} ${args[0]} failed (${code})`)
  }
}

function docker(args, options) {
  return command('docker', args, options)
}

async function inspectContainer(name) {
  const format = '{"status":{{json .State.Status}},"running":{{json .State.Running}},"exit_code":{{json .State.ExitCode}},"ports":{{json .NetworkSettings.Ports}}}'
  return decodeJSON(await docker(['inspect', '--type', 'container', '--format', format, name], { timeout: 5000 }), 'Container state')
}

async function removeContainer(name) {
  await docker(['rm', '--force', '--volumes', name], { timeout: 30000 })
  containers.delete(name)
}

async function volume(name) {
  volumes.add(name)
  await docker(['volume', 'create', '--label', `searchmeld.database-upgrade-fixture=${prefix}`, name])
  return name
}

async function helper(mounts, script) {
  const name = `${prefix}-helper-${++helperNumber}`
  containers.add(name)
  const output = await docker(['run', '--pull', 'never', '--name', name, '--network', 'none',
    '--label', `searchmeld.database-upgrade-fixture=${prefix}`,
    ...mounts.flatMap((mount) => ['--mount', mount]), '--entrypoint', 'sh', currentImage, '-euc', script])
  await removeContainer(name)
  return output
}

function request(path, { method = 'GET', headers = {}, body, timeout = 10000 } = {}) {
  const url = new URL(path, origin)
  check(url.origin === origin && url.hostname === '127.0.0.1', 'Only the loopback fixture may receive HTTP requests')
  return new Promise((resolveReply, reject) => {
    const payload = body === undefined ? undefined : JSON.stringify(body)
    const req = http.request(url, {
      method, agent: false, signal: AbortSignal.timeout(timeout),
      headers: { ...(payload === undefined ? {} : { 'Content-Type': 'application/json' }), ...headers }
    }, (res) => {
      const chunks = []
      let size = 0
      res.on('data', (chunk) => {
        size += chunk.length
        if (size > 2 * 1024 * 1024) res.destroy(new FixtureError('HTTP response exceeded its bound'))
        else chunks.push(chunk)
      })
      res.on('error', () => reject(new FixtureError('HTTP response failed')))
      res.on('end', () => resolveReply({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks).toString('utf8') }))
    })
    req.on('error', () => reject(new FixtureError('Loopback HTTP request failed or timed out')))
    req.end(payload)
  })
}

async function api(path, options = {}, status = 200, { current = false, login = false } = {}) {
  const reply = await request(path, options)
  check(reply.status === status, `${options.method || 'GET'} ${path}: unexpected HTTP status`)
  if (current && path.startsWith('/api/admin/')) {
    check(reply.headers['cache-control'] === 'no-store' && reply.headers.pragma === 'no-cache', 'Admin response must not be cached')
    if (!login) check(!reply.headers['set-cookie'], 'Only login may issue an admin Cookie')
  }
  return { ...reply, json: decodeJSON(reply.body, 'API response') }
}

async function startForwarder() {
  forwarder = http.createServer((req, res) => {
    if (!upstream) { res.writeHead(503).end(); return }
    const destination = new URL(upstream)
    const forwarded = http.request({
      hostname: destination.hostname, port: destination.port, path: req.url,
      method: req.method, headers: req.headers, agent: false, signal: AbortSignal.timeout(10000)
    }, (reply) => {
      res.writeHead(reply.statusCode, reply.headers)
      reply.on('error', () => res.destroy())
      reply.pipe(res)
    })
    forwarded.on('error', () => {
      if (!res.headersSent) res.writeHead(502)
      res.end()
    })
    req.on('aborted', () => forwarded.destroy())
    res.on('close', () => forwarded.destroy())
    req.pipe(forwarded)
  })
  forwarder.requestTimeout = 15000
  forwarder.headersTimeout = 10000
  forwarder.on('connection', (socket) => socket.setTimeout(15000, () => socket.destroy()))
  await new Promise((done, reject) => {
    forwarder.once('error', () => reject(new FixtureError('Loopback listener failed')))
    forwarder.listen(0, '127.0.0.1', done)
  })
  origin = `http://127.0.0.1:${forwarder.address().port}`
}

async function waitHealthy(name) {
  const state = await inspectContainer(name)
  const bindings = state.ports?.['80/tcp'] || []
  check(bindings.length === 1 && bindings[0].HostIp === '127.0.0.1', 'One loopback container binding is required')
  const port = Number(bindings[0].HostPort)
  check(Number.isInteger(port) && port > 0 && port <= 65535, 'Invalid container port binding')
  // Preserve the listening origin/Cookie jar; Docker may change the upstream port.
  upstream = `http://127.0.0.1:${port}`
  const deadline = Date.now() + 150000
  while (Date.now() < deadline) {
    check((await inspectContainer(name)).running, 'Application exited before becoming healthy')
    try {
      if ((await request('/healthz', { timeout: 2000 })).status === 200) return
    } catch { /* A failed probe is expected while the packaged stack starts. */ }
    await new Promise((done) => setTimeout(done, 500))
  }
  throw new FixtureError('Application startup exceeded 150 seconds')
}

async function startApp(name, image, dataVolume, environmentPath) {
  containers.add(name)
  await docker(['run', '--detach', '--pull', 'never', '--name', name, '--network', `${prefix}-network`,
    '--label', `searchmeld.database-upgrade-fixture=${prefix}`,
    '--publish', '127.0.0.1::80', '--env-file', environmentPath,
    '--mount', `type=volume,source=${dataVolume},target=/var/lib/postgresql/data,volume-nocopy`, image])
  await waitHealthy(name)
}

async function sql(name, query, { tcp = false } = {}) {
  return docker(['exec', '--interactive', '--env', 'PGCONNECT_TIMEOUT=5',
    '--env', 'PGOPTIONS=-c statement_timeout=30000 -c lock_timeout=5000', name,
    'sh', '-euc', tcp
      ? 'export PGPASSWORD="$POSTGRES_PASSWORD"; exec psql -X -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -tA'
      : 'exec psql -X -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -tA'
  ], { input: query, timeout: 45000 })
}

async function snapshot(name) {
  // No snapshots leave process memory. Audit/usage admission can append records,
  // so compare before authenticating again after each image replacement.
  return decodeJSON(await sql(name, `SELECT json_build_object(
    'admins', (SELECT json_agg(t ORDER BY id) FROM admin_users t),
    'admin_keys', (SELECT json_agg(t ORDER BY id) FROM admin_api_keys t),
    'provider_keys', (SELECT json_agg(t ORDER BY id) FROM provider_keys t),
    'tokens', (SELECT json_agg(t ORDER BY id) FROM api_tokens t),
    'providers', (SELECT json_agg(t ORDER BY id) FROM providers t),
    'settings', (SELECT json_agg(t ORDER BY key) FROM settings t),
    'requests', (SELECT json_agg(t ORDER BY id) FROM search_requests t),
    'usage', (SELECT json_agg(t ORDER BY id) FROM usage_daily t),
    'cache', (SELECT json_agg(t ORDER BY cache_key) FROM search_cache t)
  );`), 'Database snapshot')
}

async function stopApp(name) {
  // Stop PostgreSQL cleanly before making a physical backup. The entrypoint then
  // exits or is stopped; never copy a running cluster as a rollback backup.
  await docker(['exec', name, 'su-exec', 'postgres', 'pg_ctl', '-D', '/var/lib/postgresql/data',
    '-m', 'fast', '-w', '-t', '30', 'stop'], { timeout: 45000 })
  await docker(['stop', '--time', '30', name], { timeout: 45000 })
  check(!(await inspectContainer(name)).running, 'Application must stop before a volume backup')
  upstream = undefined
}

async function buildBaseline() {
  const revision = await command('git', ['rev-parse', '--verify', `${baselineRevision}^{commit}`])
  check(revision === baselineRevision, 'Historical checkout is missing; fetch the immutable baseline commit')
  const archive = join(temporary, 'baseline.tar')
  const source = join(temporary, 'baseline')
  await mkdir(source, { mode: 0o700 })
  await command('git', ['archive', '--format=tar', '--output', archive, baselineRevision])
  await command('tar', ['-xf', archive, '-C', source])
  builderCreated = true
  await docker(['buildx', 'create', '--name', prefix, '--driver', 'docker-container'])
  baselineBuilt = true
  await docker(['buildx', 'build', '--builder', prefix, '--load', '--pull', '--progress', 'plain',
    '--target', 'all-in-one', '--label', `org.opencontainers.image.revision=${baselineRevision}`,
    '--tag', `${prefix}:baseline`, source], { timeout: 15 * 60 * 1000 })
  check(await docker(['image', 'inspect', '--format', '{{index .Config.Labels "org.opencontainers.image.revision"}}', `${prefix}:baseline`]) === baselineRevision, 'Old image revision label is incorrect')
  console.log(`Built disposable historical application ${baselineRevision}`)
}

async function writeEnvironment(name, values) {
  const path = join(temporary, `${name}.env`)
  await writeFile(path, Object.entries(values).map(([key, value]) => `${key}=${value}\n`).join(''), { mode: 0o600, flag: 'wx' })
  return path
}

async function loginOld(credentials) {
  const reply = await api('/api/admin/login', { method: 'POST', body: credentials })
  check(typeof reply.json.token === 'string' && reply.json.token.startsWith('adm_'), 'Historical login must issue its old header session')
  check(!reply.headers['set-cookie'], 'Historical login must not already use Cookies')
  return reply.json.token
}

async function loginCurrent(credentials) {
  const reply = await api('/api/admin/login', {
    method: 'POST', headers: { ...proof, Origin: origin }, body: credentials
  }, 200, { current: true, login: true })
  check(isDeepStrictEqual(Object.keys(reply.json), ['expires_at']) && Number.isFinite(Date.parse(reply.json.expires_at)), 'Current login JSON must contain only an expiry')
  const cookies = reply.headers['set-cookie'] || []
  check(cookies.length === 1, 'Current login must issue exactly one Cookie')
  const attributes = cookies[0].split(';').map((item) => item.trim())
  check(attributes[0].startsWith(`${cookieName}=adm_`), 'Current login must issue the admin session Cookie')
  check(attributes.includes('Path=/api/admin') && attributes.includes('HttpOnly') && attributes.includes('SameSite=Lax'), 'Current Cookie flags are incorrect')
  check(!attributes.some((item) => /^(Secure|Domain=|Max-Age=|Expires=)/i.test(item)), 'Direct HTTP must use a host-only browser-session Cookie')
  return attributes[0]
}

async function seedOld(name, credentials) {
  const oldSession = await loginOld(credentials)
  const headers = { Authorization: `Bearer ${oldSession}` }
  const providers = (await api('/api/admin/providers', { headers })).json.providers
  check(Array.isArray(providers), 'Provider configuration must be available')
  for (const provider of providers) {
    await api(`/api/admin/providers/${provider.name}`, { method: 'PATCH', headers, body: {
      ...provider, enabled: false, base_url: 'http://127.0.0.1:9',
      settings: { ...provider.settings, upgrade_fixture: 'persisted', key_retry_count: 0 }
    } })
  }
  const adminKey = (await api('/api/admin/settings/admin-api-key', { method: 'POST', headers }, 201)).json.key
  check(typeof adminKey === 'string' && adminKey.startsWith('oak_'), 'Old application must provision an admin Key')
  const providerSecret = randomBytes(24).toString('hex')
  const serviceSecret = randomBytes(24).toString('hex')
  const key = (await api('/api/admin/keys', { method: 'POST', headers, body: {
    provider_name: 'exa', alias: 'upgrade-synthetic-key', key: providerSecret,
    exa_service_key: serviceSecret, weight: 2, rpm_limit: 60,
    daily_quota: 1000, monthly_quota: 5000, max_concurrency: 2
  } }, 201)).json
  check(Number.isSafeInteger(key.id) && key.id > 0, 'Old provider-key ID is required')
  const token = (await api('/api/admin/tokens', { method: 'POST', headers, body: {
    name: 'upgrade-synthetic-token', scopes: ['search'], allowed_providers: ['exa'],
    rate_limit_per_min: 100, daily_quota: 1000, monthly_quota: 5000
  } }, 201)).json
  check(Number.isSafeInteger(token.token?.id) && token.token.id > 0, 'Old business-token ID is required')
  check(typeof token.raw_token === 'string' && token.raw_token.startsWith('osr_'), 'Old business-token credential is required')
  const settings = (await api('/api/admin/settings', { headers })).json
  Object.assign(settings, { default_limit: 7, default_providers: ['exa'], cache_ttl_seconds: 4321, log_retention_days: 30, search_logs_limit: 73, api_auth_required: true })
  await api('/api/admin/settings', { method: 'PUT', headers, body: settings })
  // Representative persisted history/cache has no upstream dependency. All values
  // are fixed fixture literals or an API-issued, checked integer ID.
  await sql(name, `BEGIN;
    INSERT INTO search_requests (request_id, api_token_id, query, mode, providers, cache_hit, result_count, status, latency_ms, request_json, response_json)
    VALUES ('upgrade-persisted-request', ${token.token.id}, 'synthetic upgrade record', 'single', ARRAY['exa'], TRUE, 1, 'success', 12, '{"query":"synthetic upgrade record"}', '{"fixture":"persisted"}');
    INSERT INTO usage_daily (usage_date, api_token_id, requests_total, requests_success, cache_hits, results_total, latency_ms_total)
    VALUES (CURRENT_DATE, ${token.token.id}, 3, 3, 3, 3, 36);
    INSERT INTO search_cache (cache_key, response_json, expires_at)
    VALUES ('upgrade-persisted-cache', '{"fixture":"persisted"}', now() + interval '7 days');
    COMMIT;`)
  await api('/v1/search', { method: 'POST', headers: { Authorization: `Bearer ${token.raw_token}` }, body: { query: '' } }, 400)
  const deadline = Date.now() + 10000
  let tokenUsed = false
  while (Date.now() < deadline) {
    tokenUsed = await sql(name, `SELECT usage_count FROM api_tokens WHERE id=${token.token.id};`) === '1'
    if (tokenUsed) break
    await new Promise((done) => setTimeout(done, 100))
  }
  check(tokenUsed, 'Historical business token admission must persist its usage counter')
  return { oldSession, adminKey, providerSecret, serviceSecret, keyID: key.id,
    tokenID: token.token.id, businessToken: token.raw_token, settings }
}

async function verifyPreserved(name, expected, seeded, { current = false } = {}) {
  const actual = await snapshot(name)
  for (const [table, rows] of Object.entries(expected)) {
    check(isDeepStrictEqual(actual[table], rows), `Persisted ${table} changed across startup`)
  }
  check(await sql(name, 'SELECT 1;', { tcp: true }) === '1', 'Database password must still authenticate over TCP')
  const headers = { Authorization: `Bearer ${seeded.adminKey}` }
  const options = { current }
  await api('/api/admin/me', { headers }, 200, options)
  check(isDeepStrictEqual((await api('/api/admin/settings', { headers }, 200, options)).json, seeded.settings), 'Runtime settings must survive the image replacement')
  const revealedKey = (await api(`/api/admin/keys/${seeded.keyID}/secret`, { headers }, 200, options)).json
  check(revealedKey.key === seeded.providerSecret && revealedKey.exa_service_key === seeded.serviceSecret, 'Encrypted provider and service secrets must still decrypt')
  const revealedToken = (await api(`/api/admin/tokens/${seeded.tokenID}/secret`, { headers }, 200, options)).json
  check(revealedToken.token === seeded.businessToken, 'Encrypted business token must still decrypt')
  const logs = (await api('/api/admin/logs', { headers }, 200, options)).json.logs
  check(Array.isArray(logs) && logs.some((log) => log.request_id === 'upgrade-persisted-request' && log.query === 'synthetic upgrade record'), 'Historical request log must remain visible')
  const usage = (await api('/api/admin/usage/summary', { headers }, 200, options)).json
  check(usage.requests_total === 3 && usage.cache_hits === 3, 'Persisted usage must remain visible')
}

async function verifyCookieMigration(name, credentials, seeded) {
  const cookie = await loginCurrent(credentials)
  const headers = { ...proof, Cookie: cookie, Origin: origin }
  const options = { current: true }
  await api('/api/admin/me', { headers }, 200, options)
  for (const value of [seeded.oldSession, cookie.slice(`${cookieName}=`.length)]) {
    for (const credential of [{ Authorization: `Bearer ${value}` }, { 'X-API-Key': value }]) {
      await api('/api/admin/me', { headers: { ...headers, ...credential } }, 401, options)
    }
  }
  await api('/api/admin/me', { headers: { Cookie: cookie } }, 403, options)
  // Empty queries prove admission without running any provider or paid request.
  for (const [credentials, status] of [[{ Cookie: cookie }, 401], [{ Authorization: `Bearer ${seeded.businessToken}` }, 400], [{ 'X-API-Key': seeded.adminKey }, 400]]) {
    await api('/v1/search', { method: 'POST', headers: credentials, body: { query: '' } }, status)
  }
  await api('/api/admin/logout', { method: 'POST', headers }, 200, options)
  await api('/api/admin/me', { headers }, 401, options)
  const beforeRestart = await loginCurrent(credentials)
  await docker(['restart', '--time', '30', name], { timeout: 60000 })
  await waitHealthy(name)
  await api('/api/admin/me', { headers: { ...proof, Cookie: beforeRestart } }, 401, options)
  await api('/api/admin/me', { headers: { 'X-API-Key': seeded.adminKey } }, 200, options)
  await loginCurrent(credentials)
}

async function volumeFingerprint(dataVolume) {
  const output = await helper([`type=volume,source=${dataVolume},target=/pgdata,readonly,volume-nocopy`], `
    set -o pipefail
    cd /pgdata
    tar -cf - . | sha256sum
    find . -exec stat -c '%n|%u|%g|%a|%s|%y|%z' {} ';' | LC_ALL=C sort | sha256sum
  `)
  check(output.split('\n').length === 2 && output.split('\n').every((line) => /^[a-f0-9]{64}\s+-$/.test(line)), 'Volume fingerprint must include contents and ownership/mode/timestamps')
  return output
}

async function waitIncompatibleDatabase(name) {
  const deadline = Date.now() + 90000
  while (Date.now() < deadline) {
    check((await inspectContainer(name)).running, 'PostgreSQL 17 fixture exited during startup')
    try {
      // Exclude the official image's transient initialization server.
      await docker(['exec', name, 'sh', '-euc', 'test "$(cat /proc/1/comm)" = postgres; pg_isready -t 1 -U "$POSTGRES_USER"'], { timeout: 5000 })
      return
    } catch { /* The final PostgreSQL process is not ready yet. */ }
    await new Promise((done) => setTimeout(done, 500))
  }
  throw new FixtureError('PostgreSQL 17 fixture startup exceeded 90 seconds')
}

async function verifyMismatch(environmentPath) {
  const dataVolume = await volume(`${prefix}-mismatch`)
  const pgName = `${prefix}-postgres17`
  const rejectedName = `${prefix}-rejected`
  ownsIncompatibleImage = !(await docker(['image', 'ls', '--quiet', incompatibleImage]))
  await docker(['pull', incompatibleImage], { timeout: 180000 })
  containers.add(pgName)
  await docker(['run', '--detach', '--pull', 'never', '--name', pgName, '--network', 'none',
    '--label', `searchmeld.database-upgrade-fixture=${prefix}`, '--env-file', environmentPath,
    '--mount', `type=volume,source=${dataVolume},target=/var/lib/postgresql/data,volume-nocopy`, incompatibleImage])
  await waitIncompatibleDatabase(pgName)
  await sql(pgName, "CREATE TABLE upgrade_sentinel (value TEXT NOT NULL); INSERT INTO upgrade_sentinel VALUES ('synthetic-pg17-preserved');")
  await docker(['stop', '--time', '30', pgName], { timeout: 45000 })
  check((await inspectContainer(pgName)).exit_code === 0, 'PostgreSQL 17 must stop cleanly')
  const before = await volumeFingerprint(dataVolume)
  containers.add(rejectedName)
  await docker(['run', '--detach', '--pull', 'never', '--name', rejectedName, '--network', 'none',
    '--label', `searchmeld.database-upgrade-fixture=${prefix}`, '--env-file', environmentPath,
    '--mount', `type=volume,source=${dataVolume},target=/var/lib/postgresql/data,volume-nocopy`, currentImage])
  const deadline = Date.now() + 15000
  let state
  do {
    state = await inspectContainer(rejectedName)
    if (!state.running) break
    await new Promise((done) => setTimeout(done, 250))
  } while (Date.now() < deadline)
  check(!state.running && state.status === 'exited' && state.exit_code !== 0, 'Incompatible PGDATA must fail promptly')
  const rejection = await docker(['logs', '--tail', '10', rejectedName], { timeout: 5000 })
  check(rejection.includes('incompatible PG_VERSION: this image requires PostgreSQL 16; PGDATA was not modified'), 'Mismatch must report its read-only preflight failure')
  check(await volumeFingerprint(dataVolume) === before, 'Rejected cluster contents, ownership, mode or timestamps changed')
  await removeContainer(rejectedName)
  await docker(['start', pgName])
  await waitIncompatibleDatabase(pgName)
  check(await sql(pgName, 'SELECT value FROM upgrade_sentinel;') === 'synthetic-pg17-preserved', 'Matching PostgreSQL must recover the rejected cluster')
  check(await sql(pgName, 'SELECT 1;', { tcp: true }) === '1', 'Rejected cluster credentials must survive recovery')
  console.log('PostgreSQL 17 mismatch refused without modification; matching-server recovery passed')
}

async function main() {
  check(/^searchmeld-db-upgrade-[0-9]+-[0-9]+$/.test(prefix || ''), 'A run-scoped UPGRADE_CONTAINER_PREFIX is required')
  check(Boolean(process.env.RUNNER_TEMP), 'RUNNER_TEMP is required')
  check(/^[a-f0-9]{40}$/.test(process.env.GITHUB_SHA || ''), 'GITHUB_SHA is required for evidence attribution')
  check(await command('git', ['rev-parse', 'HEAD']) === process.env.GITHUB_SHA, 'Checkout must match the Actions evidence SHA')
  temporary = join(resolve(process.env.RUNNER_TEMP), prefix)
  await mkdir(temporary, { mode: 0o700 })
  ownsTemporary = true
  const imageID = await docker(['image', 'inspect', '--format', '{{.Id}}', currentImage])
  check(/^sha256:[a-f0-9]{64}$/.test(imageID), 'Current image ID is required')
  console.log(`Testing image ${imageID} from Actions checkout ${process.env.GITHUB_SHA}`)
  await buildBaseline()
  await startForwarder()
  networkCreated = true
  await docker(['network', 'create', '--driver', 'bridge', '--label', `searchmeld.database-upgrade-fixture=${prefix}`, `${prefix}-network`])
  const credentials = { username: 'upgrade-operator', password: randomBytes(24).toString('hex') }
  const environment = {
    DATABASE_MODE: 'embedded', DATABASE_URL: '', DATABASE_URL_FILE: '',
    POSTGRES_DB: database, POSTGRES_USER: database, POSTGRES_PASSWORD: randomBytes(24).toString('hex'),
    ENCRYPTION_KEY: randomBytes(32).toString('hex'), ADMIN_USERNAME: credentials.username,
    ADMIN_PASSWORD: credentials.password, ADMIN_PUBLIC_ORIGIN: '', API_AUTH_REQUIRED: 'true', MCP_ENABLED: 'false',
    TZ: 'UTC', HTTP_PROXY: '', HTTPS_PROXY: '', ALL_PROXY: '', http_proxy: '', https_proxy: '', all_proxy: ''
  }
  const oldEnvironment = await writeEnvironment('old', environment)
  // The persisted hash, not re-provisioning with an environment password, must log in.
  const currentEnvironment = await writeEnvironment('current', { ...environment, ADMIN_PASSWORD: '' })
  const dataVolume = await volume(`${prefix}-data`)
  const backupVolume = await volume(`${prefix}-backup`)
  const oldName = `${prefix}-old`
  const currentName = `${prefix}-current`
  const recoveryName = `${prefix}-recovery`
  await startApp(oldName, `${prefix}:baseline`, dataVolume, oldEnvironment)
  check(await sql(oldName, 'SHOW server_version_num;').then((value) => /^16\d{4}$/.test(value)), 'Historical bundled PostgreSQL major must be 16')
  const seeded = await seedOld(oldName, credentials)
  const expected = await snapshot(oldName)
  await verifyPreserved(oldName, expected, seeded)
  await stopApp(oldName)
  await helper([
    `type=volume,source=${dataVolume},target=/source,readonly,volume-nocopy`,
    `type=volume,source=${backupVolume},target=/backup,volume-nocopy`
  ], 'cp -a /source/. /backup/')
  await removeContainer(oldName)
  await startApp(currentName, currentImage, dataVolume, currentEnvironment)
  await verifyPreserved(currentName, expected, seeded, { current: true })
  await verifyCookieMigration(currentName, credentials, seeded)
  console.log('Historical credentials, encrypted secrets, settings, history and new Cookie transport passed')
  await stopApp(currentName)
  await removeContainer(currentName)
  await startApp(recoveryName, `${prefix}:baseline`, backupVolume, currentEnvironment)
  await verifyPreserved(recoveryName, expected, seeded)
  await loginOld(credentials)
  await stopApp(recoveryName)
  await removeContainer(recoveryName)
  console.log('Stopped pre-upgrade backup recovered under the historical image')
  await verifyMismatch(currentEnvironment)
  console.log(`Database upgrade contract passed for ${process.env.GITHUB_SHA}; baseline ${baselineRevision}`)
}

async function finish(code) {
  if (closing) return closing
  closing = (async () => {
    const cleanupDeadline = setTimeout(() => {
      console.error('Database upgrade cleanup exceeded two minutes; run the scoped Actions fallback cleanup')
      process.exit(1)
    }, 2 * 60 * 1000)
    controller.abort()
    let cleanupFailed = false
    const clean = async (args, timeout = 30000) => {
      try { await docker(args, { timeout, cleanup: true }) } catch { cleanupFailed = true }
    }
    if (forwarder) {
      try {
        forwarder.closeAllConnections()
        forwarder.close()
      } catch { cleanupFailed = true }
    }
    for (const name of containers) await clean(['rm', '--force', '--volumes', name])
    for (const name of volumes) await clean(['volume', 'rm', name])
    if (networkCreated) await clean(['network', 'rm', `${prefix}-network`])
    if (builderCreated) await clean(['buildx', 'rm', '--force', prefix], 60000)
    if (baselineBuilt) await clean(['image', 'rm', '--force', `${prefix}:baseline`])
    if (ownsIncompatibleImage) await clean(['image', 'rm', incompatibleImage])
    if (ownsTemporary) {
      try { await rm(temporary, { recursive: true, force: true }) } catch { cleanupFailed = true }
    }
    if (cleanupFailed) console.error('Database upgrade cleanup failed; run the scoped Actions fallback cleanup')
    clearTimeout(cleanupDeadline)
    process.exit(cleanupFailed ? 1 : code)
  })()
  return closing
}

for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, () => { void finish(1) })
setTimeout(() => {
  console.error('Database upgrade fixture exceeded 25 minutes')
  void finish(1)
}, 25 * 60 * 1000).unref()
main().then(() => finish(0), (error) => {
  if (!closing) console.error(error instanceof FixtureError ? error.message : 'Database upgrade fixture failed; unexpected error details withheld')
  return finish(1)
})

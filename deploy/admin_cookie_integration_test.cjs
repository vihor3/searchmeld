'use strict'

if (process.env.GITHUB_ACTIONS !== 'true') {
  throw new Error('Packaged admin Cookie checks may run only in GitHub Actions')
}

const assert = require('node:assert/strict')
const { execFile } = require('node:child_process')
const { randomBytes } = require('node:crypto')
const { mkdir, readFile, rm, writeFile } = require('node:fs/promises')
const http = require('node:http')
const https = require('node:https')
const { isIP } = require('node:net')
const { join, resolve, sep } = require('node:path')
const { promisify } = require('node:util')
const { chromium } = require('playwright')

const execute = promisify(execFile)
const cookieName = 'searchmeld_admin_session'
const proof = { 'X-SearchMeld-Admin': '1' }
const tokenKeys = ['searchmeld-admin-token', 'one-search-admin-token']
const desktop = { width: 1440, height: 1000 }
const mobile = { width: 390, height: 844 }
const target = '/providers?fixture=a%2Bb#shared'
const prefix = process.env.COOKIE_CONTAINER_PREFIX
assert.match(prefix || '', /^searchmeld-admin-cookie-(?:(?:external|all-in-one)-)?[0-9]+-[0-9]+$/)
const databaseMode = process.env.COOKIE_TEST_DATABASE_MODE || 'embedded'
assert.ok(['embedded', 'external'].includes(databaseMode), 'COOKIE_TEST_DATABASE_MODE must be embedded or external')
const image = process.env.COOKIE_TEST_IMAGE || 'searchmeld:ci-all-in-one'
const postgresImage = process.env.COOKIE_TEST_POSTGRES_IMAGE || 'postgres:16-alpine'
assert.ok(process.env.RUNNER_TEMP, 'RUNNER_TEMP is required')
assert.ok(process.env.COOKIE_TEST_TEMP, 'COOKIE_TEST_TEMP is required')
assert.ok(process.env.ARTIFACT_DIR, 'ARTIFACT_DIR is required')
const temporary = resolve(process.env.COOKIE_TEST_TEMP)
const artifacts = resolve(process.env.ARTIFACT_DIR)
for (const directory of [temporary, artifacts]) {
  assert.ok(directory.startsWith(`${resolve(process.env.RUNNER_TEMP)}${sep}`), 'Fixture files must stay in runner temporary storage')
}

const containers = new Set()
const networks = new Set()
const servers = new Set()
const contexts = new Set()
const gates = new Set()
const secrets = new Set()
const operations = new AbortController()
let browser
let cleanupPromise

/** Registers nonempty synthetic credentials for diagnostic redaction and returns the value unchanged. */
function secret(value) {
  if (value) secrets.add(value)
  return value
}

/** Masks registered credential strings in diagnostics; unregistered values are not scrubbed. */
function redact(value) {
  let text = String(value)
  for (const value of secrets) text = text.replaceAll(value, '[redacted]')
  return text
}

/** Exposes a resolver for coordinating response capture, release and delivery between fixture actions. */
function deferred() {
  let resolve
  const promise = new Promise((done) => { resolve = done })
  return { promise, resolve }
}

/** Limits the wait and clears its timer; a timeout does not cancel the underlying operation. */
async function bounded(promise, label, milliseconds = 15000) {
  let timer
  try {
    return await Promise.race([
      promise,
      new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(`Timed out: ${label}`)), milliseconds) })
    ])
  } finally {
    clearTimeout(timer)
  }
}

/**
 * Runs the Docker CLI with process/output limits and trims stdout. Cleanup
 * commands bypass the shared abort signal; aborting the CLI is not container removal.
 */
async function docker(args, { timeout = 90000, cleanup = false } = {}) {
  const { stdout } = await execute('docker', args, {
    timeout, killSignal: 'SIGKILL', maxBuffer: 1024 * 1024, signal: cleanup ? undefined : operations.signal
  })
  return stdout.trim()
}

/** Reads only lifecycle fields and port bindings, excluding environment, health output and application logs. */
async function inspectContainer(name) {
  // Never inspect Env, State.Health.Log or application logs: only lifecycle
  // status and published bindings are needed to diagnose this fixture.
  const format = '{"id":{{json .Id}},"status":{{json .State.Status}},"running":{{json .State.Running}},"started_at":{{json .State.StartedAt}},"restart_count":{{json .RestartCount}},"exit_code":{{json .State.ExitCode}},"ports":{{json .NetworkSettings.Ports}}}'
  const { stdout } = await execute('docker', ['inspect', '--type', 'container', '--format', format, name], {
    timeout: 5000, killSignal: 'SIGKILL', maxBuffer: 64 * 1024, signal: operations.signal
  })
  return JSON.parse(stdout)
}

/** Reduces request failures to a short uppercase code without exposing arbitrary error messages. */
function requestErrorCode(error) {
  return typeof error?.code === 'string' && /^[A-Z][A-Z0-9_]{0,63}$/.test(error.code) ? error.code : 'REQUEST_FAILED'
}

/**
 * Sends loopback-only JSON requests with timed aborts and a 4 MiB reply cap.
 * Self-signed TLS is accepted for this synthetic fixture, not as production policy.
 */
function request(origin, path, { method = 'GET', headers = {}, body, timeout = 10000 } = {}) {
  const url = new URL(path, origin)
  assert.equal(url.hostname, '127.0.0.1', 'Fixtures must not contact external services')
  return new Promise((resolve, reject) => {
    const transport = url.protocol === 'https:' ? https : http
    const payload = body === undefined ? undefined : JSON.stringify(body)
    const req = transport.request(url, {
      method, agent: false, rejectUnauthorized: false, signal: AbortSignal.timeout(timeout),
      headers: { ...(payload === undefined ? {} : { 'Content-Type': 'application/json' }), ...headers }
    }, (res) => {
      const chunks = []
      let size = 0
      res.on('data', (chunk) => {
        size += chunk.length
        if (size > 4 * 1024 * 1024) res.destroy(new Error('Fixture response exceeded its bound'))
        else chunks.push(chunk)
      })
      res.on('error', reject)
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks).toString('utf8') }))
    })
    req.setTimeout(timeout, () => req.destroy(Object.assign(new Error('Fixture HTTP request timed out'), { code: 'ETIMEDOUT' })))
    req.on('error', reject)
    req.end(payload)
  })
}

/** Asserts admin status/no-store headers and forbids Set-Cookie unless the caller marks a login reply. */
function adminReply(reply, status, label, { login = false } = {}) {
  assert.equal(reply.status, status, label)
  assert.equal(reply.headers['cache-control'], 'no-store', `${label}: no-store`)
  assert.equal(reply.headers.pragma, 'no-cache', `${label}: no-cache`)
  if (!login) assert.ok(!reply.headers['set-cookie'], `${label}: only login may set a Cookie`)
  return reply
}

/**
 * Reports restricted app/database state and caller-supplied probe summaries,
 * without querying Env, health logs or application logs.
 */
async function healthDiagnostics(fixture, phase, lastHealthError) {
  let state
  try {
    state = await inspectContainer(fixture.name)
  } catch (error) {
    state = { inspect_error: requestErrorCode(error) }
  }
  let databaseState
  if (fixture.database) {
    try {
      databaseState = await inspectContainer(fixture.database.name)
    } catch (error) {
      databaseState = { inspect_error: requestErrorCode(error) }
    }
  }
  console.error(`Packaged ${fixture.label} ${phase} health diagnostics: ${JSON.stringify({
    ...state, database: databaseState, browser_origin: fixture.origin, upstream: fixture.upstream || null, last_health_error: lastHealthError
  })}`)
}

/**
 * Refreshes the Docker upstream without changing the browser origin, then polls
 * health on a 120-second deadline and emits restricted diagnostics on failure.
 */
async function waitHealthy(fixture, phase) {
  let lastHealthError = { error: 'NOT_PROBED' }
  try {
    const state = await inspectContainer(fixture.name)
    const bindings = state.ports?.['80/tcp'] || []
    assert.equal(bindings.length, 1, 'One published fixture binding is required')
    const [binding] = bindings
    assert.equal(binding.HostIp, '127.0.0.1')
    const port = Number(binding.HostPort)
    assert.ok(Number.isInteger(port) && port > 0 && port <= 65535, 'Valid published fixture port')
    const previousUpstream = fixture.upstream || null
    fixture.upstream = `http://127.0.0.1:${port}`
    console.log(`Packaged ${fixture.label} ${phase} mapping: ${JSON.stringify({
      browser_origin: fixture.origin, previous_upstream: previousUpstream, upstream: fixture.upstream
    })}`)

    const deadline = Date.now() + 120000
    while (Date.now() < deadline) {
      fixture.upstreamHealthError = null
      try {
        const response = await request(fixture.origin, '/healthz', { timeout: Math.max(1, Math.min(10000, deadline - Date.now())) })
        if (response.status === 200) return
        lastHealthError = { status: response.status, upstream_error: fixture.upstreamHealthError }
      } catch (error) {
        lastHealthError = { error: requestErrorCode(error), upstream_error: fixture.upstreamHealthError }
      }
      await new Promise((resolve) => setTimeout(resolve, Math.max(0, Math.min(500, deadline - Date.now()))))
    }
    throw new Error(`Packaged ${fixture.label} instance did not become healthy within 120 seconds (${phase})`)
  } catch (error) {
    await healthDiagnostics(fixture, phase, lastHealthError)
    throw error
  }
}

/** Tracks a loopback listener for suite cleanup, bounds its startup wait and sets request/socket timeouts. */
async function listen(server) {
  servers.add(server)
  server.requestTimeout = 15000
  server.headersTimeout = 10000
  server.on('connection', (socket) => {
    socket.setTimeout(15000, () => socket.destroy())
  })
  await bounded(new Promise((resolve, reject) => {
    server.once('error', reject)
    server.listen(0, '127.0.0.1', resolve)
  }), 'loopback listener')
  return server.address().port
}

/** Writes synthetic environment values with owner-only creation permissions; suite cleanup removes the directory. */
async function writeEnvironment(path, environment) {
  await writeFile(path, Object.entries(environment).map(([key, value]) => `${key}=${value}\n`).join(''), { mode: 0o600 })
}

/**
 * Tracks an internal-network PostgreSQL fixture without host bindings, installs
 * its synthetic DSN in the app environment and polls TCP readiness on a 60-second deadline.
 */
async function startExternalDatabase(fixture, environment) {
  fixture.database = { name: `${fixture.name}-postgres`, network: `${fixture.name}-network` }
  const { name, network } = fixture.database
  networks.add(network)
  await docker(['network', 'create', '--internal', '--label', `searchmeld.admin-cookie-fixture=${prefix}`, network])
  const environmentPath = join(temporary, `${fixture.mode}-postgres.env`)
  await writeEnvironment(environmentPath, {
    POSTGRES_DB: environment.POSTGRES_DB, POSTGRES_USER: environment.POSTGRES_USER, POSTGRES_PASSWORD: environment.POSTGRES_PASSWORD
  })
  containers.add(name)
  await docker(['run', '--detach', '--pull', 'never', '--name', name,
    '--label', `searchmeld.admin-cookie-fixture=${prefix}`, '--log-driver', 'none',
    '--network', network, '--network-alias', 'cookie-postgres', '--env-file', environmentPath, postgresImage])

  const url = new URL('postgresql://cookie-postgres:5432/cookie_fixture?sslmode=disable')
  url.username = environment.POSTGRES_USER
  url.password = environment.POSTGRES_PASSWORD
  environment.DATABASE_URL = secret(url.href)
  const deadline = Date.now() + 60000
  while (Date.now() < deadline) {
    const state = await inspectContainer(name)
    assert.equal(state.running, true, 'External PostgreSQL must stay running during startup')
    assert.ok(Object.values(state.ports || {}).every((bindings) => !bindings?.length), 'External PostgreSQL must not publish host ports')
    try {
      // The image's temporary initialization server is socket-only; require
      // TCP readiness so it cannot race the application's migration startup.
      await docker(['exec', name, 'pg_isready', '--host=127.0.0.1', '--username=cookie_fixture', '--dbname=cookie_fixture', '--timeout=2'], {
        timeout: Math.max(1, Math.min(5000, deadline - Date.now()))
      })
      return
    } catch {
      await new Promise((resolve) => setTimeout(resolve, Math.max(0, Math.min(500, deadline - Date.now()))))
    }
  }
  await healthDiagnostics(fixture, 'database startup', { error: 'DATABASE_NOT_READY' })
  throw new Error('External PostgreSQL did not become ready within 60 seconds')
}

/**
 * Records external container/postmaster identities and asserts one persisted
 * account, Key and Token for restart comparisons; embedded mode returns null.
 */
async function externalDatabaseSnapshot(fixture) {
  if (!fixture.database) return null
  const state = await inspectContainer(fixture.database.name)
  assert.equal(state.running, true, 'External PostgreSQL must remain running')
  const snapshot = JSON.parse(await docker(['exec', fixture.database.name, 'psql', '-X', '--no-password',
    '--username=cookie_fixture', '--dbname=cookie_fixture', '--tuples-only', '--no-align', '--set=ON_ERROR_STOP=1',
    '--command', `SELECT json_build_object(
      'started_at', pg_postmaster_start_time(),
      'admin_users', (SELECT count(*) FROM admin_users),
      'admin_api_keys', (SELECT count(*) FROM admin_api_keys),
      'api_tokens', (SELECT count(*) FROM api_tokens)
    )`], { timeout: 5000 }))
  assert.equal(snapshot.admin_users, 1, 'The application account must live in the external database')
  assert.equal(snapshot.admin_api_keys, 1, 'The installed Key must live in the external database')
  assert.equal(snapshot.api_tokens, 1, 'The ordinary Token must live in the external database')
  return { id: state.id, started_at: state.started_at, restart_count: state.restart_count, database: snapshot }
}

/**
 * Removes this fixture's app, optional database, anonymous volumes and network,
 * forgetting each only after success. Listeners remain owned by suite cleanup.
 */
async function removeFixture(fixture) {
  await docker(['rm', '--force', '--volumes', fixture.name], { timeout: 10000 })
  containers.delete(fixture.name)
  if (fixture.database) {
    await docker(['rm', '--force', '--volumes', fixture.database.name], { timeout: 10000 })
    containers.delete(fixture.database.name)
    await docker(['network', 'rm', fixture.database.network], { timeout: 10000 })
    networks.delete(fixture.database.network)
  }
}

/**
 * Starts a synthetic-credential packaged instance behind a stable HTTP/HTTPS
 * origin. HTTPS first tests missing public-origin rejection, then configured
 * proxy access; created Docker resources and listeners stay tracked for cleanup.
 */
async function startFixture(mode, tls) {
  const fixture = { mode, label: `${databaseMode}-${mode}`, name: `${prefix}-${mode}`, problems: [], observations: [], pages: [], pendingObservations: [] }
  fixture.credentials = { username: 'ci-cookie-operator', password: secret(randomBytes(24).toString('hex')) }
  // Keep the browser's actual listening origin alive across container restarts.
  // Docker's dynamically published upstream is re-inspected after every start;
  // all traffic still traverses the built assets and bundled Nginx -> Go.
  /**
   * Preserves Host/port while targeting the current Docker binding. HTTPS
   * sanitizes proxy headers; HTTP retains spoof inputs for bundled Nginx tests.
   */
  const forward = (req, res) => {
    if (!fixture.upstream) { res.writeHead(503).end(); return }
    const headers = { ...req.headers, host: req.headers.host }
    if (mode === 'https') {
      for (const name of ['forwarded', 'true-client-ip', 'x-real-ip', 'x-forwarded-for', 'x-forwarded-host', 'x-forwarded-port']) delete headers[name]
      headers['x-forwarded-proto'] = 'https'
    }
    // HTTP forwards spoof-test headers unchanged to the bundled proxy.
    const destination = new URL(fixture.upstream)
    const forwarded = http.request({ hostname: destination.hostname, port: destination.port, path: req.url, method: req.method, headers, agent: false }, (reply) => {
      res.writeHead(reply.statusCode, reply.headers)
      reply.on('error', () => res.destroy())
      reply.pipe(res)
    })
    forwarded.setTimeout(10000, () => forwarded.destroy(Object.assign(new Error('Fixture upstream request timed out'), { code: 'ETIMEDOUT' })))
    forwarded.on('error', (error) => {
      if (req.url === '/healthz') fixture.upstreamHealthError = requestErrorCode(error)
      if (!res.headersSent) res.writeHead(502)
      res.end()
    })
    req.on('aborted', () => forwarded.destroy())
    res.on('close', () => forwarded.destroy())
    req.pipe(forwarded)
  }
  const proxy = mode === 'https' ? https.createServer(tls, forward) : http.createServer(forward)
  fixture.origin = `${mode}://127.0.0.1:${await listen(proxy)}`
  assert.ok(new URL(fixture.origin).port, 'Exercise a non-default Host port')
  const environment = {
    APP_ENV: 'production', DATABASE_MODE: databaseMode, DATABASE_URL: '',
    POSTGRES_DB: 'cookie_fixture', POSTGRES_USER: 'cookie_fixture',
    POSTGRES_PASSWORD: secret(randomBytes(24).toString('hex')),
    ENCRYPTION_KEY: secret(randomBytes(32).toString('hex')),
    ADMIN_USERNAME: fixture.credentials.username, ADMIN_PASSWORD: fixture.credentials.password,
    ADMIN_PUBLIC_ORIGIN: '', ADMIN_LOGIN_MAX_ATTEMPTS: '5',
    API_AUTH_REQUIRED: 'true', MCP_ENABLED: 'true',
    CORS_ALLOWED_ORIGINS: mode === 'https' ? `*,${fixture.origin}` : '*',
    HTTP_PROXY: '', HTTPS_PROXY: '', ALL_PROXY: '', http_proxy: '', https_proxy: '', all_proxy: ''
  }
  if (databaseMode === 'external') await startExternalDatabase(fixture, environment)
  const environmentPath = join(temporary, `${mode}.env`)
  const origins = mode === 'https' ? ['', fixture.origin] : ['']
  for (const publicOrigin of origins) {
    environment.ADMIN_PUBLIC_ORIGIN = publicOrigin
    await writeEnvironment(environmentPath, environment)
    // Both modes retain their database across app restart. All anonymous
    // database volumes belong to this fixture and are removed with --volumes.
    containers.add(fixture.name)
    await docker(['create', '--pull', 'never', '--name', fixture.name,
      '--label', `searchmeld.admin-cookie-fixture=${prefix}`, '--log-driver', 'none',
      ...(fixture.database ? ['--network', fixture.database.network] : []),
      '--publish', '127.0.0.1::80', '--env-file', environmentPath, image])
    // Internal-only networks do not reliably publish host ports. Attach the
    // app (never PostgreSQL) to the existing bridge before starting it.
    if (fixture.database) await docker(['network', 'connect', 'bridge', fixture.name])
    await docker(['start', fixture.name])
    await waitHealthy(fixture, mode === 'https' && !publicOrigin ? 'missing-origin startup' : 'startup')
    for (const packageName of ['curl', 'libcurl', 'nghttp2-libs']) {
      await assert.rejects(() => docker(['exec', fixture.name, 'apk', 'info', '-e', packageName], { timeout: 10000 }),
        (error) => error.code === 1, `Unneeded runtime package must be absent: ${packageName}`)
    }
    const probe = ['exec', '--env', 'http_proxy=http://127.0.0.1:9', '--env', 'HTTP_PROXY=http://127.0.0.1:9',
      fixture.name, 'timeout', '-s', 'KILL', '4', 'wget', '-Y', 'off', '-T', '4', '-q', '-O', '/dev/null']
    await docker([...probe, 'http://127.0.0.1/healthz'], { timeout: 10000 })
    await assert.rejects(() => docker([...probe, 'http://127.0.0.1/v1/providers'], { timeout: 10000 }),
      (error) => error.code === 1, 'Packaged health client must bypass proxies and reject HTTP errors')
    if (mode === 'https' && !publicOrigin) {
      adminReply(await request(fixture.origin, '/api/admin/login', {
        method: 'POST', headers: { ...proof, Origin: fixture.origin }, body: fixture.credentials
      }), 403, 'Allowlisted HTTPS Origin without public-origin configuration must not downgrade Cookie security')
      await docker(['rm', '--force', '--volumes', fixture.name])
      containers.delete(fixture.name)
      fixture.upstream = undefined
    }
  }

  /** Serves a synthetic page on another port of the same Cookie host for browser Origin/proof tests. */
  const hostileHandler = (_req, res) => {
    res.writeHead(200, { 'Content-Type': 'text/html', 'Cache-Control': 'no-store' })
    res.end('<!doctype html><title>Synthetic same-site origin</title><p>CI origin fixture</p>')
  }
  const hostile = mode === 'https' ? https.createServer(tls, hostileHandler) : http.createServer(hostileHandler)
  fixture.hostileOrigin = `${mode}://127.0.0.1:${await listen(hostile)}`
  return fixture
}

/**
 * Tracks an isolated Cookie jar with fixture-only routing, credential-presence
 * observations and redacted page errors. The TLS exception is fixture-only;
 * callers or suite cleanup close the context.
 */
async function openContext(fixture, viewport = desktop) {
  const context = await browser.newContext({ viewport, ignoreHTTPSErrors: true, serviceWorkers: 'block', reducedMotion: 'reduce' })
  contexts.add(context)
  context.setDefaultTimeout(15000)
  context.setDefaultNavigationTimeout(20000)
  await context.route('**/*', async (route) => {
    const origin = new URL(route.request().url()).origin
    if (![fixture.origin, fixture.hostileOrigin].includes(origin)) {
      fixture.problems.push('Unexpected external browser request')
      await route.abort()
    } else await route.continue()
  })
  context.on('page', (page) => {
    fixture.pages.push(page)
    page.on('pageerror', (error) => {
      // A rejected refresh is allowed to propagate from the existing view.
      if (error.message !== 'admin login required') fixture.problems.push(redact(error.message))
    })
    page.on('request', (req) => {
      if (!new URL(req.url()).pathname.startsWith('/api/admin/')) return
      fixture.pendingObservations.push(req.allHeaders().then((headers) => {
        fixture.observations.push({
          path: new URL(req.url()).pathname, method: req.method(),
          headerCredential: 'authorization' in headers || 'x-api-key' in headers,
          cookie: Boolean(headers.cookie?.includes(`${cookieName}=`)),
          proof: headers['x-searchmeld-admin'], origin: headers.origin
        })
      }).catch(() => { fixture.problems.push('Could not inspect browser request headers') }))
    })
  })
  return context
}

/**
 * Uses real page Cookie/CORS handling for a credentialed fetch. Fetch failures,
 * including but not limited to CORS rejection, return a blocked marker.
 */
async function browserRequest(page, url, { method = 'GET', headers = proof, body } = {}) {
  return page.evaluate(async ({ url, method, headers, body }) => {
    try {
      const response = await fetch(url, {
        method, headers, credentials: 'include', signal: AbortSignal.timeout(10000),
        ...(body === undefined ? {} : { body: JSON.stringify(body) })
      })
      return { status: response.status, body: await response.text(), headers: Object.fromEntries(response.headers) }
    } catch {
      return { blocked: true }
    }
  }, { url, method, headers, body })
}

/** Checks the login return target and absence of the protected shell once the login card appears. */
async function atLogin(page, redirect = target) {
  await page.locator('.login-card').waitFor()
  const url = new URL(page.url())
  assert.equal(url.pathname, '/login')
  assert.equal(url.searchParams.get('redirect'), redirect)
  assert.equal(await page.locator('.app-shell').count(), 0, 'Protected shell must not mount anonymously')
}

/** Waits for the destination and protected view, asserting that no login card remains mounted. */
async function atProtected(page, fixture, path = target) {
  await page.waitForURL(new URL(path, fixture.origin).href)
  await page.locator('.app-shell').waitFor()
  if (new URL(path, fixture.origin).pathname === '/providers') await page.locator('.provider-grid').waitFor()
  assert.equal(await page.locator('.login-card').count(), 0)
}

/**
 * Registers the sole admin Cookie for redaction before asserting its host, path,
 * HttpOnly/SameSite, protocol-dependent Secure and browser-session lifetime flags,
 * then returns it for subsequent session checks.
 */
async function currentCookie(context, fixture) {
  const cookies = (await context.cookies(`${fixture.origin}/api/admin/me`)).filter((cookie) => cookie.name === cookieName)
  assert.equal(cookies.length, 1, 'One shared admin Cookie')
  const cookie = cookies[0]
  secret(cookie.value)
  assert.equal(cookie.domain, '127.0.0.1')
  assert.equal(cookie.path, '/api/admin')
  assert.equal(cookie.httpOnly, true)
  assert.equal(cookie.sameSite, 'Lax')
  assert.equal(cookie.secure, fixture.mode === 'https')
  assert.equal(cookie.expires, -1, 'Browser-session Cookie, not a persistent credential')
  return cookie
}

/**
 * Submits synthetic account credentials through the UI. The wrong-password case
 * must stay on the login form; success returns the Cookie after expiry-only JSON
 * and protected-route checks.
 */
async function login(page, fixture, { wrongPassword = false } = {}) {
  const inputs = page.locator('.login-card input')
  await inputs.nth(0).fill(fixture.credentials.username)
  await inputs.nth(1).fill(wrongPassword ? secret(`${fixture.credentials.password}-wrong`) : fixture.credentials.password)
  const pending = page.waitForResponse((res) => new URL(res.url()).pathname === '/api/admin/login' && res.request().method() === 'POST')
  await page.locator('.login-card .el-button').click()
  const response = await pending
  await response.finished()
  const reply = { status: response.status(), headers: await response.allHeaders() }
  adminReply(reply, wrongPassword ? 401 : 200, 'password login', { login: !wrongPassword })
  if (wrongPassword) {
    await page.locator('.login-card .el-button.is-loading').waitFor({ state: 'hidden' })
    await atLogin(page)
    await page.locator('.el-message--error').waitFor()
    return
  }
  const cookie = await currentCookie(page.context(), fixture)
  const body = await response.json()
  assert.deepEqual(Object.keys(body), ['expires_at'], 'No session credential in login JSON')
  assert.ok(Number.isFinite(Date.parse(body.expires_at)))
  const setCookie = reply.headers['set-cookie'] || ''
  assert.ok(setCookie.startsWith(`${cookieName}=`))
  assert.ok(!/;\s*(domain|max-age|expires)=/i.test(setCookie), 'Host-only session Cookie')
  await atProtected(page, fixture)
  return cookie
}

/**
 * Asserts legacy keys are absent and registered credentials do not appear in
 * snapshots of sessionStorage, localStorage or document.cookie.
 */
async function storageIsClean(page) {
  const state = await page.evaluate(() => ({ session: { ...sessionStorage }, local: { ...localStorage }, visibleCookies: document.cookie }))
  for (const key of tokenKeys) {
    assert.ok(!(key in state.session) && !(key in state.local), 'Legacy credential storage must be deleted')
  }
  const serialized = JSON.stringify(state)
  for (const value of secrets) assert.ok(!serialized.includes(value), 'Credentials must not be visible in JS storage or document.cookie')
  assert.ok(!state.visibleCookies.includes(`${cookieName}=`), 'Admin Cookie must remain HttpOnly')
}

/** Writes a synthetic full-page PNG while masking known credential-bearing controls and code elements. */
async function screenshot(page, name) {
  await mkdir(artifacts, { recursive: true })
  await page.screenshot({
    path: join(artifacts, `${name}.png`), fullPage: true,
    mask: [page.locator('input'), page.locator('code'), page.locator('.raw-token-row'), page.locator('.token-copy-button')]
  })
}

/** Probes live server session state through the page's Cookie/proof path and checks admin response policy. */
async function me(page, fixture, status = 200) {
  const reply = await browserRequest(page, `${fixture.origin}/api/admin/me`)
  adminReply(reply, status, 'browser me')
  if (status === 200) assert.equal(typeof JSON.parse(reply.body).username, 'string')
}

/** Returns the real reply after empty-query validation, permitting entry correlation without provider work. */
async function searchValidation(fixture, headers, label) {
  // Empty input reaches the retained business handler but returns before
  // orchestration, so an accepted Key/Token cannot spend upstream credits.
  const reply = await request(fixture.origin, '/v1/search', { method: 'POST', headers, body: {} })
  assert.equal(reply.status, 400, label)
  const body = JSON.parse(reply.body)
  assert.equal(body.error.status, 400, `${label}: business validation envelope`)
  assert.equal(body.error.message, 'query is required', `${label}: reached the search handler`)
  return reply
}

/** Captures server identity and expected safe fields; only rejected Extract cases select an execution here. */
function userRequestCase(reply, fields = {}, rejectedExtract = false) {
  const requestID = reply.headers['x-request-id']
  assert.ok(typeof requestID === 'string' && requestID.length > 0 && Buffer.byteLength(requestID) <= 256, 'Mounted Go reply needs a bounded server request ID')
  return {
    request_id: requestID, operation: 'search', compat_format: 'native', method: 'POST', path: '/v1/search',
    auth_type: 'unknown', api_token_id: null, token_name: '', http_status: reply.status,
    completion: 'completed', mcp_error_count: 0, mcp_tool_error_count: 0,
    execution_request_id: rejectedExtract ? requestID : null, ...fields
  }
}

/** Rejects credential/payload fields and invalid metadata without including secret values in assertion messages. */
function safeUserRequest(log) {
  assert.ok(log !== null && typeof log === 'object' && !Array.isArray(log), 'User entry must be an object')
  const serialized = JSON.stringify(log)
  for (const value of secrets) assert.ok(!serialized.includes(value), 'Entry metadata must exclude synthetic credentials and payload sentinels')
  assert.deepEqual(Object.keys(log).sort(), [
    'id', 'request_id', 'created_at', 'operation', 'compat_format', 'method', 'path', 'client_ip',
    'auth_type', 'api_token_id', 'token_name', 'http_status', 'completion', 'latency_ms',
    'mcp_error_count', 'mcp_tool_error_count', 'execution_request_id'
  ].sort(), 'Entry API contains only the frozen metadata fields')
  assert.ok(Number.isSafeInteger(log.id) && log.id > 0, 'Entry has a persistent numeric ID')
  assert.ok(Number.isFinite(Date.parse(log.created_at)), 'Entry has a request timestamp')
  assert.ok(Number.isSafeInteger(log.latency_ms) && log.latency_ms >= 0, 'Entry duration includes zero')
  assert.ok(isIP(log.client_ip) > 0, 'Packaged proxy attribution must be an IP without a port')
  assert.ok(!['192.0.2.44', '198.51.100.44', '203.0.113.44'].includes(log.client_ip), 'Forged forwarding headers cannot choose the entry IP')
}

/**
 * Polls for post-handler inserts against a ten-second deadline, failing HTTP errors
 * immediately. Resolves exact details and checks rejected Extract links have no calls.
 */
async function userRequestEntries(fixture, headers, cases) {
  assert.equal(new Set(cases.map((entry) => entry.request_id)).size, cases.length, 'Every HTTP request has a distinct server ID')
  const deadline = Date.now() + 10000
  let logs
  while (Date.now() < deadline) {
    const reply = adminReply(await request(fixture.origin, '/api/admin/request-logs?limit=1000', {
      headers, timeout: Math.max(1, Math.min(2000, deadline - Date.now()))
    }), 200, 'Read persisted user entries')
    const body = JSON.parse(reply.body)
    assert.deepEqual(Object.keys(body), ['logs'], 'Entry list envelope')
    assert.ok(Array.isArray(body.logs), 'Entry list must be an array')
    logs = body.logs
    if (cases.every((entry) => logs.some((log) => log.request_id === entry.request_id))) break
    await new Promise((done) => setTimeout(done, Math.max(0, Math.min(100, deadline - Date.now()))))
  }
  assert.ok(cases.every((entry) => logs?.some((log) => log.request_id === entry.request_id)), 'User entries must persist within ten seconds of handling')
  assert.equal(logs.length, cases.length, 'Admin reads, health probes and removed routes must not create user entries')
  for (const log of logs) safeUserRequest(log)
  const details = []
  let clientIP
  for (const expected of cases) {
    const matching = logs.filter((log) => log.request_id === expected.request_id)
    assert.equal(matching.length, 1, 'One entry per server request ID')
    const [log] = matching
    safeUserRequest(log)
    for (const [field, value] of Object.entries(expected)) assert.equal(log[field], value, `Entry ${field} must reflect the request outcome`)
    if (clientIP === undefined) clientIP = log.client_ip
    assert.equal(log.client_ip, clientIP, 'Clean and forged requests retain the same packaged proxy peer')
    const detail = JSON.parse(adminReply(await request(fixture.origin, `/api/admin/request-logs/${log.id}`, { headers }), 200, 'User entry detail').body)
    assert.deepEqual(Object.keys(detail).sort(), ['execution_log_id', 'log'], 'Entry detail envelope')
    safeUserRequest(detail.log)
    assert.deepEqual(detail.log, log, 'List and detail expose the same persisted metadata')
    if (expected.execution_request_id === null) {
      assert.equal(detail.execution_log_id, null, 'No selected execution must have an explicit null link')
    } else {
      assert.ok(Number.isSafeInteger(detail.execution_log_id) && detail.execution_log_id > 0, 'Rejected Extract must resolve its existing execution')
      const execution = JSON.parse(adminReply(await request(fixture.origin, `/api/admin/logs/${detail.execution_log_id}`, { headers }), 200, 'Linked rejected Extract execution').body)
      assert.equal(execution.log.request_id, expected.execution_request_id, 'Execution linkage is exact')
      assert.equal(execution.log.operation, 'extract')
      assert.equal(execution.log.status, 'error')
      assert.deepEqual(execution.calls, [], 'Rejected Extract does not call a provider')
      assert.deepEqual(execution.log.request_json, {}, 'Rejected Extract retains no request payload')
      assert.deepEqual(execution.log.response_json, {}, 'Rejected Extract retains no response payload')
    }
    details.push(detail)
  }
  return details
}

/** Checks populated list/detail reads inherit Cookie proof, explicit Key precedence and no-store policy. */
async function userRequestAccess(fixture, cookieHeaders, { key, token }, detail) {
  for (const path of ['/api/admin/request-logs', `/api/admin/request-logs/${detail.log.id}`]) {
    for (const headers of [cookieHeaders, { Authorization: `Bearer ${key}` }, { 'X-API-Key': key }]) {
      const body = JSON.parse(adminReply(await request(fixture.origin, path, { headers }), 200, 'Admin-only user entry read').body)
      if (Array.isArray(body.logs)) assert.ok(body.logs.some((log) => log.request_id === detail.log.request_id), 'Authorized list contains the real entry')
      else assert.deepEqual(body, detail, 'Key-only and Cookie detail agree')
    }
    for (const headers of [
      proof, { Authorization: `Bearer ${token}` }, { 'X-API-Key': token },
      { ...cookieHeaders, Authorization: `Bearer ${token}` }, { ...cookieHeaders, 'X-API-Key': token },
      { ...cookieHeaders, Authorization: 'Bearer oak_invalid_fixture' }
    ]) adminReply(await request(fixture.origin, path, { headers }), 401, 'Ordinary Tokens and invalid headers cannot read entries or borrow Cookie privileges')
    adminReply(await request(fixture.origin, path, { headers: { Cookie: cookieHeaders.Cookie } }), 403, 'Entry Cookie read requires browser proof')
  }
}

/** Inspects real packaged metadata at the page viewport, then returns to providers for the session lifecycle cases. */
async function userRequestUI(fixture, page, detail, size) {
  await page.goto(`${fixture.origin}/logs`)
  await atProtected(page, fixture, '/logs')
  const tabs = page.locator('.logs-view-tabs')
  const userTab = tabs.getByRole('tab', { name: '\u7528\u6237\u8bf7\u6c42', exact: true })
  await userTab.waitFor()
  assert.equal(await userTab.getAttribute('aria-selected'), 'true', 'User requests are the default view')
  await tabs.getByRole('tab', { name: '\u6267\u884c\u65e5\u5fd7', exact: true }).waitFor()
  const toggle = page.locator('.logs-actions .el-switch')
  if (await toggle.locator('[role="switch"]').getAttribute('aria-checked') === 'true') await toggle.click()
  const row = page.locator(`[data-log-kind="user"][data-log-id="${detail.log.id}"]`)
  await row.waitFor()
  const pending = page.waitForResponse((res) => new URL(res.url()).pathname === `/api/admin/request-logs/${detail.log.id}` && res.request().method() === 'GET')
  await row.click()
  const response = await pending
  adminReply({ status: response.status(), headers: await response.allHeaders() }, 200, 'Packaged drawer reads with browser Cookie')
  await response.finished()
  assert.deepEqual(await response.json(), detail, 'Browser detail matches the persisted API entry')
  const drawer = page.locator('.log-drawer.open')
  await drawer.waitFor()
  await drawer.locator('.entry-detail[aria-busy="false"]').waitFor()
  await drawer.locator('[data-execution-state="none"]').waitFor()
  await page.waitForFunction((log) => {
    const text = document.querySelector('.log-drawer.open')?.textContent || ''
    return [log.request_id, log.path, log.method, log.client_ip, log.token_name, `Token #${log.api_token_id}`, `HTTP ${log.http_status}`].every((value) => text.includes(value))
  }, detail.log)
  const text = await drawer.innerText()
  for (const value of secrets) assert.ok(!text.includes(value), 'Drawer must not render credentials or unretained payloads')
  const inBounds = await drawer.evaluate((element) => {
    const rect = element.getBoundingClientRect()
    return rect.left >= -1 && rect.right <= innerWidth + 1 && element.scrollWidth <= element.clientWidth + 1
  })
  assert.ok(inBounds, 'Packaged entry drawer fits the viewport')
  await screenshot(page, `${fixture.label}-${size}-user-request`)
  await drawer.press('Escape')
  await drawer.waitFor({ state: 'hidden' })
  await page.goto(`${fixture.origin}${target}`)
  await atProtected(page, fixture)
}

/**
 * Checks removed public routes across credentials and persisted auth-on/off modes,
 * returns entry expectations for retained business probes and restores settings in finally.
 */
async function removedPublicEndpoints(fixture, cookieHeaders, { key, token, session }) {
  /** Pins settings requests to the authenticated Cookie/proof headers while business auth mode changes. */
  const admin = (path, options = {}) => request(fixture.origin, path, { ...options, headers: cookieHeaders })
  const settings = JSON.parse(adminReply(await admin('/api/admin/settings'), 200, 'Read effective business auth setting').body)
  assert.equal(settings.api_auth_required, true, 'Fixture business auth must initially be enabled in the database')
  const credentials = [
    ['anonymous', {}], ['Cookie', cookieHeaders],
    ['session Bearer', { Authorization: `Bearer ${session}` }], ['session X-API-Key', { 'X-API-Key': session }],
    ['Token Bearer', { Authorization: `Bearer ${token}` }], ['Token X-API-Key', { 'X-API-Key': token }],
    ['Key Bearer', { Authorization: `Bearer ${key}` }], ['Key X-API-Key', { 'X-API-Key': key }],
    ['invalid Key', { Authorization: 'Bearer oak_invalid_fixture' }]
  ]
  let settingsChanged = false
  const entries = []
  try {
    for (const required of [true, false]) {
      if (!required) {
        settingsChanged = true
        adminReply(await admin('/api/admin/settings', {
          method: 'PUT', body: { ...settings, api_auth_required: false }
        }), 200, 'Temporarily disable effective business auth')
      }
      const effective = JSON.parse(adminReply(await admin('/api/admin/settings'), 200, 'Probe effective business auth').body)
      assert.equal(effective.api_auth_required, required)
      if (required) {
        const reply = await request(fixture.origin, '/v1/search', { method: 'POST', body: {} })
        assert.equal(reply.status, 401, 'Auth-on rejects anonymous business calls')
        entries.push(userRequestCase(reply))
      } else entries.push(userRequestCase(await searchValidation(fixture, {}, 'Auth-off reaches the retained business handler anonymously'), { auth_type: 'anonymous' }))
      for (const path of ['/v1/providers', '/v1/usage/summary']) {
        for (const [label, headers] of credentials) {
          const reply = await request(fixture.origin, path, { headers })
          assert.equal(reply.status, 404, `${path}: removed for ${label}, auth_required=${required}`)
          assert.ok(reply.body === '404 page not found\n', `${path}: no configuration or usage payload in removed-route response`)
        }
      }
      for (const headers of [cookieHeaders, { Authorization: `Bearer ${key}` }, { 'X-API-Key': key }]) {
        const providers = adminReply(await request(fixture.origin, '/api/admin/providers', { headers }), 200, 'Admin providers remain available')
        assert.ok(JSON.parse(providers.body).providers.length > 0, 'Admin provider configuration remains populated')
        const usage = adminReply(await request(fixture.origin, '/api/admin/usage/summary', { headers }), 200, 'Admin usage remains available')
        assert.equal(typeof JSON.parse(usage.body).requests_total, 'number', 'Admin instance usage retains its schema')
      }
      for (const path of ['/api/admin/providers', '/api/admin/usage/summary']) {
        for (const headers of [proof, { Authorization: `Bearer ${token}` }]) {
          adminReply(await request(fixture.origin, path, { headers }), 401, 'Business auth mode never opens admin metadata')
        }
      }
    }
  } finally {
    if (settingsChanged) adminReply(await admin('/api/admin/settings', { method: 'PUT', body: settings }), 200, 'Restore business auth before remaining browser cases')
  }
  const restored = JSON.parse(adminReply(await admin('/api/admin/settings'), 200, 'Probe restored business auth').body)
  assert.equal(restored.api_auth_required, true)
  return entries
}

/**
 * Provisions synthetic Key/Token credentials and tests header precedence,
 * Cookie exclusion from business/MCP auth, browser proof and Origin policy,
 * including same-host cross-port requests. Returns credentials and persisted entry
 * snapshots for restart checks; all business cases reject before provider work.
 */
async function authBoundaries(fixture, page) {
  const cookie = await currentCookie(page.context(), fixture)
  const cookieHeaders = { ...proof, Cookie: `${cookieName}=${cookie.value}`, Origin: fixture.origin }
  /** Overlays case-specific headers on a valid Cookie/proof baseline to test credential and Origin rejection. */
  const admin = (path, options = {}) => request(fixture.origin, path, { ...options, headers: { ...cookieHeaders, ...options.headers } })
  const rotation = adminReply(await admin('/api/admin/settings/admin-api-key', { method: 'POST' }), 201, 'first Key provisioning')
  const key = secret(JSON.parse(rotation.body).key)
  assert.ok(key.startsWith('oak_'))
  const tokenReply = adminReply(await admin('/api/admin/tokens', {
    method: 'POST', body: { name: 'ci-cookie-control', scopes: ['search'], allowed_providers: [] }
  }), 201, 'ordinary Token provisioning')
  const provisioned = JSON.parse(tokenReply.body)
  const token = secret(provisioned.raw_token)
  assert.ok(token.startsWith('osr_'))
  assert.ok(Number.isSafeInteger(provisioned.token?.id) && provisioned.token.id > 0, 'Ordinary Token has a persistent ID')
  const caller = { auth_type: 'api_token', api_token_id: provisioned.token.id, token_name: provisioned.token.name }
  const requestCases = [userRequestCase(await searchValidation(fixture, { Authorization: `Bearer ${token}` }, 'Ordinary search Token reaches business validation'), caller)]
  requestCases.push(...await removedPublicEndpoints(fixture, cookieHeaders, { key, token, session: cookie.value }))

  for (const headers of [
    { Authorization: `Bearer ${cookie.value}` }, { 'X-API-Key': cookie.value },
    { Authorization: `Bearer ${token}` }, { Authorization: 'Bearer oak_invalid_fixture' },
    { Authorization: '' }, { Authorization: 'Bearer ' }, { 'X-API-Key': '' },
    { Authorization: 'Basic invalid' },
    { Authorization: 'Bearer oak_invalid_fixture', 'X-API-Key': key }
  ]) {
    adminReply(await admin('/api/admin/me', { headers }), 401, 'Selected invalid header cannot borrow a Cookie')
  }
  adminReply(await request(fixture.origin, '/api/admin/me', { headers: proof }), 401, 'Unauthenticated me')
  for (const headers of [{ Authorization: `Bearer ${key}` }, { 'X-API-Key': key }]) {
    adminReply(await request(fixture.origin, '/api/admin/me', { headers }), 200, 'Key-only CLI without proof or Origin')
    adminReply(await request(fixture.origin, '/api/admin/me', {
      headers: { ...headers, Cookie: `${cookieName}=adm_invalid_fixture` }
    }), 200, 'Valid Key wins over stale Cookie')
    requestCases.push(userRequestCase(await searchValidation(fixture, headers, 'Key business API'), { auth_type: 'admin_key' }))
    adminReply(await request(fixture.origin, '/api/admin/logout', {
      method: 'POST', headers: { ...headers, Cookie: cookieHeaders.Cookie }
    }), 200, 'Key logout is non-revoking and ignores incidental Cookie')
    await me(page, fixture)
    adminReply(await request(fixture.origin, '/api/admin/me', { headers }), 200, 'Key survives its logout')
  }

  // Send the Cookie explicitly beyond its browser Path. Path omission alone
  // would not establish the backend's public credential boundary.
  const business = [
    ['/v1/search', 'search', 'native'], ['/v1/extract', 'extract', 'native'],
    ['/v1/compat/tavily/search', 'search', 'tavily'], ['/v1/compat/tavily/extract', 'extract', 'tavily'],
    ['/v1/compat/serper/search', 'search', 'serper'], ['/v1/compat/openai/responses-search', 'search', 'openai']
  ]
  for (const [path, operation, compat_format] of business) {
    for (const [kind, headers] of [
      ['Cookie', cookieHeaders], ['session Bearer', { Authorization: `Bearer ${cookie.value}` }], ['session X-API-Key', { 'X-API-Key': cookie.value }]
    ]) {
      const reply = await request(fixture.origin, path, {
        method: 'POST', headers, body: { query: 'ci-only', urls: ['https://example.invalid/article'] }
      })
      assert.equal(reply.status, 401, `${path}: explicit ${kind} must not authorize business calls`)
      requestCases.push(userRequestCase(reply, { path, operation, compat_format }))
    }
  }
  const rpc = { jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'ci-no-such-tool', arguments: {} } }
  for (const path of ['/mcp', '/v1/mcp']) {
    for (const headers of [cookieHeaders, { Authorization: `Bearer ${cookie.value}` }]) {
      const reply = await request(fixture.origin, path, { method: 'POST', headers, body: rpc })
      assert.equal(reply.status, 401, 'MCP rejects session credentials')
      requestCases.push(userRequestCase(reply, { path, operation: 'mcp', mcp_error_count: 1 }))
    }
    const reply = await request(fixture.origin, path, { method: 'POST', headers: { Authorization: `Bearer ${key}` }, body: rpc })
    assert.equal(reply.status, 200, 'Key passes MCP auth without calling a provider')
    assert.equal(JSON.parse(reply.body).error.code, -32602, 'Synthetic unknown tool reaches method validation')
    requestCases.push(userRequestCase(reply, { path, operation: 'mcp', auth_type: 'admin_key', mcp_error_count: 1 }))
  }

  const payloadSentinel = secret(`ci-entry-private-${randomBytes(16).toString('hex')}`)
  const spoofed = await request(fixture.origin, `/v1/search?api_key=${payloadSentinel}`, {
    method: 'POST', body: { query: '', unretained: payloadSentinel },
    headers: {
      Authorization: `Bearer ${key}`, Cookie: cookieHeaders.Cookie, 'X-Request-ID': 'ci-entry-correlation',
      'True-Client-IP': '198.51.100.44', 'X-Real-IP': '203.0.113.44', 'X-Forwarded-For': '192.0.2.44',
      Forwarded: 'for=192.0.2.44;proto=https;host=untrusted.invalid'
    }
  })
  assert.equal(spoofed.status, 400, 'Spoofed metadata fixture still stops at empty-query validation')
  assert.ok(spoofed.headers['x-request-id']?.startsWith('ci-entry-correlation-'), 'Permitted correlation prefix remains intact')
  requestCases.push(userRequestCase(spoofed, { auth_type: 'admin_key' }))
  for (const [path, compat_format] of [['/v1/extract', 'native'], ['/v1/compat/tavily/extract', 'tavily']]) {
    const reply = await request(fixture.origin, path, {
      method: 'POST', headers: compat_format === 'tavily' ? {} : { Authorization: `Bearer ${token}` },
      body: { urls: [`https://example.invalid/${payloadSentinel}`], ...(compat_format === 'tavily' ? { api_key: token } : {}) }
    })
    assert.equal(reply.status, 403, 'Search-only Token cannot execute Extract')
    assert.equal(JSON.parse(reply.body).error.message, 'api token does not include extract scope', 'Extract rejects before decoding targets')
    requestCases.push(userRequestCase(reply, { ...caller, path, operation: 'extract', compat_format }, true))
  }
  try {
    adminReply(await admin(`/api/admin/tokens/${provisioned.token.id}`, {
      method: 'PATCH', body: { ...provisioned.token, rate_limit_per_min: 1 }
    }), 200, 'Limit the synthetic Token for a bounded 429 case')
    const admitted = await searchValidation(fixture, { Authorization: `Bearer ${token}` }, 'First limited Token request reaches validation')
    requestCases.push(userRequestCase(admitted, caller))
    const limited = await request(fixture.origin, '/v1/search', { method: 'POST', headers: { Authorization: `Bearer ${token}` }, body: {} })
    assert.equal(limited.status, 429, 'Second request exceeds the one-per-minute Token limit')
    assert.equal(JSON.parse(limited.body).error.message, 'api token rate limit exceeded', '429 is generated by mounted Go auth')
    requestCases.push(userRequestCase(limited, caller))
  } finally {
    adminReply(await admin(`/api/admin/tokens/${provisioned.token.id}`, {
      method: 'PATCH', body: provisioned.token
    }), 200, 'Restore the Token rate setting before restart checks')
  }
  const requestLogs = await userRequestEntries(fixture, cookieHeaders, requestCases)
  await userRequestAccess(fixture, cookieHeaders, { key, token }, requestLogs[0])
  const executions = JSON.parse(adminReply(await admin('/api/admin/logs?limit=1000'), 200, 'Execution history remains separate').body).logs
  assert.deepEqual(executions.map((log) => log.request_id).sort(), requestCases.filter((entry) => entry.execution_request_id !== null).map((entry) => entry.execution_request_id).sort(), 'Only the existing rejected Extract paths create executions')
  const usage = JSON.parse(adminReply(await admin('/api/admin/usage/summary'), 200, 'Entry-only traffic does not bill providers').body)
  assert.equal(usage.requests_total, 0, 'Entry logging and rejected Extract must not create accounted executions')

  for (const suppliedProof of [undefined, '0']) {
    const headers = suppliedProof === undefined ? {} : { 'X-SearchMeld-Admin': suppliedProof }
    for (const [path, method] of [['/api/admin/me', 'GET'], ['/api/admin/logout', 'POST'], ['/api/admin/login', 'POST']]) {
      adminReply(await browserRequest(page, `${fixture.origin}${path}`, {
        method, headers: { 'Content-Type': 'application/json', ...headers },
        ...(path.endsWith('/login') ? { body: fixture.credentials } : {})
      }), 403, 'Missing/wrong browser proof')
    }
  }
  await me(page, fixture)

  const preflight = {
    'Access-Control-Request-Method': 'POST',
    'Access-Control-Request-Headers': 'content-type,x-searchmeld-admin'
  }
  const allowed = adminReply(await request(fixture.origin, '/api/admin/logout', {
    method: 'OPTIONS', headers: { Origin: fixture.origin, ...preflight }
  }), 204, 'Canonical preflight with non-default Host port')
  assert.equal(allowed.headers['access-control-allow-origin'], fixture.origin)
  assert.equal(allowed.headers['access-control-allow-credentials'], 'true')
  assert.match(allowed.headers.vary, /\borigin\b/i)
  assert.match(allowed.headers['access-control-allow-headers'], /x-searchmeld-admin/i)
  for (const origin of [fixture.hostileOrigin, 'null', `${fixture.origin}/path`]) {
    for (const [path, method] of [['/api/admin/me', 'GET'], ['/api/admin/logout', 'POST'], ['/api/admin/login', 'POST']]) {
      const denied = adminReply(await admin(path, {
        method, headers: { Origin: origin }, ...(path.endsWith('/login') ? { body: fixture.credentials } : {})
      }), 403, 'Untrusted supplied Origin')
      assert.ok(!denied.headers['access-control-allow-origin'], 'Do not reflect hostile/null Origin')
    }
    adminReply(await admin('/api/admin/me', { headers: { Origin: origin, Authorization: `Bearer ${key}` } }), 403, 'Key cannot bypass supplied Origin')
    const denied = adminReply(await request(fixture.origin, '/api/admin/logout', {
      method: 'OPTIONS', headers: { Origin: origin, ...preflight }
    }), 403, 'Hostile preflight; wildcard CORS is not admin trust')
    assert.ok(!denied.headers['access-control-allow-origin'])
  }

  // A different port is a different origin but the SAME site and Cookie host.
  // Simple requests really carry the shared Cookie; SameSite cannot stop them.
  const hostile = await page.context().newPage()
  await hostile.goto(fixture.hostileOrigin)
  const metadataBefore = JSON.parse((await admin('/api/admin/settings/admin-api-key')).body)
  for (const [path, method] of [
    ['/api/admin/me', 'GET'], ['/api/admin/logout', 'POST'], ['/api/admin/settings/admin-api-key', 'POST']
  ]) {
    const seen = hostile.waitForRequest((req) => new URL(req.url()).pathname === path && req.method() === method)
    const result = await browserRequest(hostile, `${fixture.origin}${path}`, { method, headers: {} })
    assert.equal(result.blocked, true, 'Hostile same-site browser cannot read admin replies')
    const sent = await (await seen).allHeaders()
    assert.equal(sent.origin, fixture.hostileOrigin)
    assert.ok(sent.cookie?.includes(`${cookieName}=`), 'Prove ambient Cookie was present on the hostile simple request')
    await me(page, fixture)
  }
  assert.deepEqual(JSON.parse((await admin('/api/admin/settings/admin-api-key')).body), metadataBefore, 'Hostile simple write did not rotate the Key')
  const preflighted = await browserRequest(hostile, `${fixture.origin}/api/admin/logout`, { method: 'POST' })
  assert.equal(preflighted.blocked, true, 'Hostile custom-header fetch is rejected by CORS')
  await me(page, fixture)
  await hostile.close()
  return { key, token, requestCases, requestLogs }
}

/**
 * Captures one real reply per path, delaying delivery rather than server
 * authentication. Callers own release(), which unroutes the hold; suite cleanup
 * resolves outstanding release gates before closing browser resources.
 */
async function holdResponses(fixture, page, paths, status) {
  const arrived = deferred()
  const released = deferred()
  const settled = deferred()
  const remaining = new Set(paths)
  let captured = 0
  let completed = 0
  let failure
  gates.add(released)
  /** Selects endpoint pathnames; the handler's remaining set limits each to one captured reply. */
  const matcher = (url) => paths.includes(url.pathname)
  /**
   * Fetches before awaiting release, fulfilling and disposing the reply on success;
   * capture/delivery failures are recorded and route abort is attempted.
   */
  const handler = async (route) => {
    const path = new URL(route.request().url()).pathname
    if (!remaining.delete(path)) { await route.continue(); return }
    try {
      // Fetch the real protected response now, then delay only its delivery.
      const response = await route.fetch({ timeout: 10000, maxRedirects: 0 })
      adminReply({ status: response.status(), headers: response.headers() }, status, 'Held real admin response')
      captured += 1
      if (captured === paths.length) arrived.resolve()
      await released.promise
      await route.fulfill({ response })
      await response.dispose()
    } catch (error) {
      failure = error
      fixture.problems.push(redact(error.message))
      arrived.resolve()
      await route.abort().catch(() => {})
    } finally {
      completed += 1
      if (completed === paths.length) settled.resolve()
    }
  }
  await page.route(matcher, handler)
  return {
    /** Waits for all captures or a saved failure within the bound, without canceling outstanding fetches. */
    async wait() { await bounded(arrived.promise, 'real response capture'); if (failure) throw failure },
    /** Releases replies, waits for handlers, then removes the gate/route and reports saved failures. */
    async release() {
      released.resolve()
      await bounded(settled.promise, 'held response delivery')
      gates.delete(released)
      await page.unroute(matcher, handler)
      if (failure) throw failure
    }
  }
}

/** Triggers the visible providers toolbar refresh to exercise its request and loading lifecycle. */
async function refresh(page) {
  await page.locator('.page-actions .el-button[title="\u5237\u65b0"]').click()
}

/**
 * Holds old-session 401/logout replies while a same-jar page logs in again, then
 * awaits action settlement and server probes before checking the newer Cookie survives.
 */
async function staleResponses(fixture, first, second) {
  for (const kind of ['401', 'logout']) {
    const old = await currentCookie(first.context(), fixture)
    if (kind === '401') {
      adminReply(await request(fixture.origin, '/api/admin/logout', {
        method: 'POST', headers: { ...proof, Cookie: `${cookieName}=${old.value}` }
      }), 200, 'Revoke old session before stale 401')
    }
    const hold = await holdResponses(fixture, first, kind === '401' ? ['/api/admin/providers', '/api/admin/keys'] : ['/api/admin/logout'], kind === '401' ? 401 : 200)
    if (kind === '401') await refresh(first)
    else await first.locator('.float-logout').click()
    await hold.wait()
    await second.reload()
    await atLogin(second)
    const newer = await login(second, fixture)
    assert.ok(newer.value !== old.value, 'New login must mint another session')
    const reprobe = first.waitForResponse((res) => new URL(res.url()).pathname === '/api/admin/me' && res.status() === 200)
    await hold.release()
    await reprobe
    // Wait for the originating action's loading/finally path, not only HTTP
    // headers; then perform another real navigation from the affected page.
    await first.waitForFunction(() => !document.querySelector('.float-logout')?.disabled && !document.querySelector('.page-actions .el-button.is-loading'))
    await atProtected(first, fixture, kind === 'logout' ? '/playground' : target)
    assert.ok((await currentCookie(first.context(), fixture)).value === newer.value, 'Old response must not clear the newer Cookie')
    await me(first, fixture)
    await me(second, fixture)
    await first.goto(`${fixture.origin}${target}`)
    await atProtected(first, fixture)
  }
}

/** Checks forged forwarding headers cannot reset one nonexistent account's lockout, without locking the shared account. */
async function lockoutDespiteSpoofing(fixture) {
  // Run last, and use a different nonexistent username so the shared account's
  // successful-login and restart cases can never inherit the lockout bucket.
  const body = { username: 'ci-forwarded-ip-lockout', password: secret(randomBytes(16).toString('hex')) }
  for (let attempt = 1; attempt <= 7; attempt += 1) {
    const reply = await request(fixture.origin, '/api/admin/login', {
      method: 'POST', body,
      headers: {
        ...proof, Origin: fixture.origin,
        'True-Client-IP': `198.51.100.${attempt}`, 'X-Real-IP': `203.0.113.${attempt}`,
        'X-Forwarded-For': `192.0.2.${attempt}`, 'X-Forwarded-Host': 'untrusted.invalid',
        'X-Forwarded-Port': '443', 'X-Forwarded-Proto': 'https',
        Forwarded: `for=192.0.2.${attempt};proto=https;host=untrusted.invalid`
      }
    })
    adminReply(reply, attempt < 5 ? 401 : 429, 'Forged forwarding headers cannot choose login buckets')
  }
}

/**
 * Exercises shared/separate jars, Cookie deletion/revocation, stale replies and
 * restart with the original browser origin and jar. External DB identity and
 * installed credentials and entry metadata must persist; contexts close on success.
 * The caller owns browser/container teardown; failures retain tracked resources.
 */
async function exercise(fixture) {
  const context = await openContext(fixture)
  const first = await context.newPage()
  const second = await context.newPage()
  // Neither page is a popup/opener clone. Only the browser Cookie jar is shared.
  await first.goto(`${fixture.origin}${target}`)
  await atLogin(first)
  await login(first, fixture, { wrongPassword: true })
  await screenshot(first, `${fixture.label}-desktop-login-error`)
  await login(first, fixture)
  await first.locator('.el-message--error').waitFor({ state: 'hidden' })
  await second.goto(`${fixture.origin}${target}`)
  await atProtected(second, fixture)
  await second.reload()
  await atProtected(second, fixture)
  await me(first, fixture)
  await me(second, fixture)
  await storageIsClean(first)
  await storageIsClean(second)
  assert.equal(await first.locator('.float-mark').evaluate((img) => img.complete && img.naturalWidth > 0), true, 'Packaged logo asset renders')
  await screenshot(first, `${fixture.label}-desktop-shared-login`)
  await second.setViewportSize(mobile)
  await screenshot(second, `${fixture.label}-mobile-shared-login`)

  const independent = await openContext(fixture, mobile)
  const anonymous = await independent.newPage()
  await anonymous.goto(`${fixture.origin}${target}`)
  await atLogin(anonymous)
  await me(anonymous, fixture, 401)
  await me(first, fixture)
  await independent.close()
  contexts.delete(independent)
  const { key, token, requestCases, requestLogs } = await authBoundaries(fixture, first)
  await userRequestUI(fixture, first, requestLogs[0], 'desktop')
  await userRequestUI(fixture, second, requestLogs[0], 'mobile')

  const formerlyValid = await currentCookie(context, fixture)
  for (const page of [first, second]) {
    await page.evaluate(({ keys, value }) => {
      for (const key of keys) { sessionStorage.setItem(key, value); localStorage.setItem(key, value) }
    }, { keys: tokenKeys, value: formerlyValid.value })
  }
  await context.clearCookies({ name: cookieName })
  for (const page of [first, second]) {
    await me(page, fixture, 401)
    await refresh(page)
    await atLogin(page)
    await page.reload()
    await atLogin(page)
    await storageIsClean(page)
  }
  await screenshot(second, `${fixture.label}-mobile-cookie-deleted`)
  await login(first, fixture)
  await second.reload()
  await atProtected(second, fixture)

  const beforeLogout = await currentCookie(context, fixture)
  const loggedOut = first.waitForResponse((res) => new URL(res.url()).pathname === '/api/admin/logout')
  await first.locator('.float-logout').click()
  const response = await loggedOut
  adminReply({ status: response.status(), headers: await response.allHeaders() }, 200, 'Explicit UI logout')
  await atLogin(first, null)
  assert.ok((await currentCookie(context, fixture)).value === beforeLogout.value, 'Logout revokes server state without clearing the Cookie')
  await me(second, fixture, 401)
  await refresh(second)
  await atLogin(second)
  adminReply(await request(fixture.origin, '/api/admin/me', { headers: { Authorization: `Bearer ${key}` } }), 200, 'Browser logout leaves installed Key valid')
  await first.goto(`${fixture.origin}${target}`)
  await atLogin(first)
  await login(first, fixture)
  await second.reload()
  await atProtected(second, fixture)
  await staleResponses(fixture, first, second)

  const browserOrigin = fixture.origin
  const beforeRestart = await currentCookie(context, fixture)
  const beforeDatabase = await externalDatabaseSnapshot(fixture)
  try {
    await docker(['restart', '--time', '5', fixture.name])
  } catch (error) {
    await healthDiagnostics(fixture, 'restart command failed', { error: 'NOT_PROBED' })
    throw error
  }
  await waitHealthy(fixture, 'after restart')
  assert.deepEqual(await userRequestEntries(fixture, { 'X-API-Key': key }, requestCases), requestLogs, 'Entry metadata and exact execution links survive app restart')
  if (fixture.database) {
    assert.deepEqual(await externalDatabaseSnapshot(fixture), beforeDatabase, 'App restart must not replace/restart external PostgreSQL or lose persisted account/Keys')
  }
  assert.equal(fixture.origin, browserOrigin, 'Restart must retain the browser public origin')
  assert.ok((await currentCookie(context, fixture)).value === beforeRestart.value, 'Restart rejection must use the existing Cookie jar')
  for (const page of [first, second]) {
    assert.equal(new URL(page.url()).origin, browserOrigin, 'Existing pages keep their origin across restart')
    await me(page, fixture, 401)
    await refresh(page)
    await atLogin(page)
  }
  adminReply(await request(fixture.origin, '/api/admin/me', { headers: { Authorization: `Bearer ${key}` } }), 200, 'Installed Key survives restart while memory sessions do not')
  await searchValidation(fixture, { Authorization: `Bearer ${key}` }, 'Installed Key retains business access after restart')
  await searchValidation(fixture, { Authorization: `Bearer ${token}` }, 'Persisted ordinary Token retains business access after restart')
  await screenshot(first, `${fixture.label}-desktop-restarted-session`)
  await login(first, fixture)
  await second.reload()
  await atProtected(second, fixture)
  await storageIsClean(first)
  await storageIsClean(second)
  await lockoutDespiteSpoofing(fixture)
  await Promise.all(fixture.pendingObservations)
  assert.ok(fixture.observations.length > 0)
  assert.ok(fixture.observations.every((entry) => !entry.headerCredential), 'Packaged browser must never attach a stored header credential')
  assert.ok(fixture.observations.some((entry) => entry.path === '/api/admin/me' && entry.cookie && entry.proof === '1'))
  assert.deepEqual(fixture.problems, [], 'Unexpected browser/fixture errors')
  await context.close()
  contexts.delete(context)
}

/**
 * Coalesces teardown: aborts normal Docker CLI operations, releases gates and
 * closes tracked contexts/browser/listeners. Attempts removal of tracked containers
 * with anonymous volumes, tracked networks and the fixture temporary directory.
 * Failed steps mark a nonzero exit; timing out does not prove a resource has exited.
 */
async function cleanup() {
  if (cleanupPromise) return cleanupPromise
  cleanupPromise = (async () => {
    operations.abort()
    const failures = []
    /** Records sync failures, rejections or five-second wait expiry so later cleanup steps can be attempted. */
    const step = async (label, action) => {
      try {
        await bounded(action(), label, 5000)
      } catch {
        failures.push(label)
      }
    }
    for (const gate of gates) gate.resolve()
    for (const context of contexts) await step('context cleanup', () => context.close())
    if (browser) await step('browser cleanup', () => browser.close())
    for (const server of servers) {
      server.closeAllConnections()
      await step('listener cleanup', () => new Promise((resolve) => server.close(resolve)))
    }
    for (const name of containers) {
      await step(`container ${name}`, () => docker(['rm', '--force', '--volumes', name], { timeout: 5000, cleanup: true }))
    }
    for (const name of networks) {
      await step(`network ${name}`, () => docker(['network', 'rm', name], { timeout: 5000, cleanup: true }))
    }
    await step('temporary file cleanup', () => rm(temporary, { recursive: true, force: true }))
    if (failures.length) {
      console.error(`Packaged fixture cleanup incomplete: ${failures.join(', ')}`)
      process.exitCode = 1
    }
  })()
  return cleanupPromise
}

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.once(signal, () => {
    console.error(`Packaged fixture interrupted by ${signal}`)
    const forcedExit = setTimeout(() => process.exit(1), 90000)
    cleanup().finally(() => { clearTimeout(forcedExit); process.exit(1) })
  })
}

/**
 * Generates runner-local TLS material and runs each HTTP/HTTPS suite under a
 * watchdog. Each browser starts after fixture startup and closes before removal;
 * failures retain tracked resources for masked PNGs and final cleanup.
 */
async function main() {
  const watchdog = setTimeout(() => process.kill(process.pid, 'SIGTERM'), 9 * 60 * 1000)
  let fixture
  try {
    await mkdir(temporary, { recursive: true, mode: 0o700 })
    // Certificates are generated only on the remote runner and never uploaded.
    await execute('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
      '-subj', '/CN=127.0.0.1', '-addext', 'subjectAltName=IP:127.0.0.1',
      '-keyout', join(temporary, 'tls.key'), '-out', join(temporary, 'tls.crt')], { timeout: 30000 })
    const tls = { key: await readFile(join(temporary, 'tls.key')), cert: await readFile(join(temporary, 'tls.crt')) }
    for (const mode of ['http', 'https']) {
      fixture = await startFixture(mode, tls)
      // Keep Chromium outside fixture network setup/removal, but retain this
      // browser and its Cookie jars through the app restart inside exercise.
      browser = await chromium.launch({ headless: true })
      await exercise(fixture)
      await bounded(browser.close(), 'browser cleanup', 5000)
      browser = undefined
      await removeFixture(fixture)
      console.log(`Packaged ${fixture.label} Cookie, shared-tab, CSRF, Key, removed-route and stale-response cases completed`)
    }
  } catch (error) {
    for (const [index, page] of (fixture?.pages || []).entries()) {
      if (!page.isClosed()) await bounded(screenshot(page, `${fixture.label}-failure-${index}`), 'failure screenshot', 5000).catch(() => {})
    }
    throw error
  } finally {
    clearTimeout(watchdog)
    await cleanup()
  }
}

main().catch((error) => { console.error(redact(error.stack || error)); process.exitCode = 1 })

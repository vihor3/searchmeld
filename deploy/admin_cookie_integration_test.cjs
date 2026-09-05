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

function secret(value) {
  if (value) secrets.add(value)
  return value
}

function redact(value) {
  let text = String(value)
  for (const value of secrets) text = text.replaceAll(value, '[redacted]')
  return text
}

function deferred() {
  let resolve
  const promise = new Promise((done) => { resolve = done })
  return { promise, resolve }
}

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

async function docker(args, { timeout = 90000, cleanup = false } = {}) {
  const { stdout } = await execute('docker', args, {
    timeout, killSignal: 'SIGKILL', maxBuffer: 1024 * 1024, signal: cleanup ? undefined : operations.signal
  })
  return stdout.trim()
}

async function inspectContainer(name) {
  // Never inspect Env, State.Health.Log or application logs: only lifecycle
  // status and published bindings are needed to diagnose this fixture.
  const format = '{"id":{{json .Id}},"status":{{json .State.Status}},"running":{{json .State.Running}},"started_at":{{json .State.StartedAt}},"restart_count":{{json .RestartCount}},"exit_code":{{json .State.ExitCode}},"ports":{{json .NetworkSettings.Ports}}}'
  const { stdout } = await execute('docker', ['inspect', '--type', 'container', '--format', format, name], {
    timeout: 5000, killSignal: 'SIGKILL', maxBuffer: 64 * 1024, signal: operations.signal
  })
  return JSON.parse(stdout)
}

function requestErrorCode(error) {
  return typeof error?.code === 'string' && /^[A-Z][A-Z0-9_]{0,63}$/.test(error.code) ? error.code : 'REQUEST_FAILED'
}

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

function adminReply(reply, status, label, { login = false } = {}) {
  assert.equal(reply.status, status, label)
  assert.equal(reply.headers['cache-control'], 'no-store', `${label}: no-store`)
  assert.equal(reply.headers.pragma, 'no-cache', `${label}: no-cache`)
  if (!login) assert.ok(!reply.headers['set-cookie'], `${label}: only login may set a Cookie`)
  return reply
}

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

async function writeEnvironment(path, environment) {
  await writeFile(path, Object.entries(environment).map(([key, value]) => `${key}=${value}\n`).join(''), { mode: 0o600 })
}

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

async function startFixture(mode, tls) {
  const fixture = { mode, label: `${databaseMode}-${mode}`, name: `${prefix}-${mode}`, problems: [], observations: [], pages: [], pendingObservations: [] }
  fixture.credentials = { username: 'ci-cookie-operator', password: secret(randomBytes(24).toString('hex')) }
  // Keep the browser's actual listening origin alive across container restarts.
  // Docker's dynamically published upstream is re-inspected after every start;
  // all traffic still traverses the built assets and bundled Nginx -> Go.
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
    if (mode === 'https' && !publicOrigin) {
      adminReply(await request(fixture.origin, '/api/admin/login', {
        method: 'POST', headers: { ...proof, Origin: fixture.origin }, body: fixture.credentials
      }), 403, 'Allowlisted HTTPS Origin without public-origin configuration must not downgrade Cookie security')
      await docker(['rm', '--force', '--volumes', fixture.name])
      containers.delete(fixture.name)
      fixture.upstream = undefined
    }
  }

  const hostileHandler = (_req, res) => {
    res.writeHead(200, { 'Content-Type': 'text/html', 'Cache-Control': 'no-store' })
    res.end('<!doctype html><title>Synthetic same-site origin</title><p>CI origin fixture</p>')
  }
  const hostile = mode === 'https' ? https.createServer(tls, hostileHandler) : http.createServer(hostileHandler)
  fixture.hostileOrigin = `${mode}://127.0.0.1:${await listen(hostile)}`
  return fixture
}

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

async function atLogin(page, redirect = target) {
  await page.locator('.login-card').waitFor()
  const url = new URL(page.url())
  assert.equal(url.pathname, '/login')
  assert.equal(url.searchParams.get('redirect'), redirect)
  assert.equal(await page.locator('.app-shell').count(), 0, 'Protected shell must not mount anonymously')
}

async function atProtected(page, fixture, path = target) {
  await page.waitForURL(new URL(path, fixture.origin).href)
  await page.locator('.app-shell').waitFor()
  if (new URL(path, fixture.origin).pathname === '/providers') await page.locator('.provider-grid').waitFor()
  assert.equal(await page.locator('.login-card').count(), 0)
}

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

async function storageIsClean(page) {
  const state = await page.evaluate(() => ({ session: { ...sessionStorage }, local: { ...localStorage }, visibleCookies: document.cookie }))
  for (const key of tokenKeys) {
    assert.ok(!(key in state.session) && !(key in state.local), 'Legacy credential storage must be deleted')
  }
  const serialized = JSON.stringify(state)
  for (const value of secrets) assert.ok(!serialized.includes(value), 'Credentials must not be visible in JS storage or document.cookie')
  assert.ok(!state.visibleCookies.includes(`${cookieName}=`), 'Admin Cookie must remain HttpOnly')
}

async function screenshot(page, name) {
  await mkdir(artifacts, { recursive: true })
  await page.screenshot({
    path: join(artifacts, `${name}.png`), fullPage: true,
    mask: [page.locator('input'), page.locator('code'), page.locator('.raw-token-row'), page.locator('.token-copy-button')]
  })
}

async function me(page, fixture, status = 200) {
  const reply = await browserRequest(page, `${fixture.origin}/api/admin/me`)
  adminReply(reply, status, 'browser me')
  if (status === 200) assert.equal(typeof JSON.parse(reply.body).username, 'string')
}

async function searchValidation(fixture, headers, label) {
  // Empty input reaches the retained business handler but returns before
  // orchestration, so an accepted Key/Token cannot spend upstream credits.
  const reply = await request(fixture.origin, '/v1/search', { method: 'POST', headers, body: {} })
  assert.equal(reply.status, 400, label)
  const body = JSON.parse(reply.body)
  assert.equal(body.error.status, 400, `${label}: business validation envelope`)
  assert.equal(body.error.message, 'query is required', `${label}: reached the search handler`)
}

async function removedPublicEndpoints(fixture, cookieHeaders, { key, token, session }) {
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
        assert.equal((await request(fixture.origin, '/v1/search', { method: 'POST', body: {} })).status, 401, 'Auth-on rejects anonymous business calls')
      } else await searchValidation(fixture, {}, 'Auth-off reaches the retained business handler anonymously')
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
}

async function authBoundaries(fixture, page) {
  const cookie = await currentCookie(page.context(), fixture)
  const cookieHeaders = { ...proof, Cookie: `${cookieName}=${cookie.value}`, Origin: fixture.origin }
  const admin = (path, options = {}) => request(fixture.origin, path, { ...options, headers: { ...cookieHeaders, ...options.headers } })
  const rotation = adminReply(await admin('/api/admin/settings/admin-api-key', { method: 'POST' }), 201, 'first Key provisioning')
  const key = secret(JSON.parse(rotation.body).key)
  assert.ok(key.startsWith('oak_'))
  const tokenReply = adminReply(await admin('/api/admin/tokens', {
    method: 'POST', body: { name: 'ci-cookie-control', scopes: ['search'], allowed_providers: [] }
  }), 201, 'ordinary Token provisioning')
  const token = secret(JSON.parse(tokenReply.body).raw_token)
  assert.ok(token.startsWith('osr_'))
  await searchValidation(fixture, { Authorization: `Bearer ${token}` }, 'Ordinary search Token reaches business validation')
  await removedPublicEndpoints(fixture, cookieHeaders, { key, token, session: cookie.value })

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
    await searchValidation(fixture, headers, 'Key business API')
    adminReply(await request(fixture.origin, '/api/admin/logout', {
      method: 'POST', headers: { ...headers, Cookie: cookieHeaders.Cookie }
    }), 200, 'Key logout is non-revoking and ignores incidental Cookie')
    await me(page, fixture)
    adminReply(await request(fixture.origin, '/api/admin/me', { headers }), 200, 'Key survives its logout')
  }

  // Send the Cookie explicitly beyond its browser Path. Path omission alone
  // would not establish the backend's public credential boundary.
  const business = [
    ['/v1/search', 'POST'], ['/v1/extract', 'POST'],
    ['/v1/compat/tavily/search', 'POST'], ['/v1/compat/tavily/extract', 'POST'],
    ['/v1/compat/serper/search', 'POST'], ['/v1/compat/openai/responses-search', 'POST']
  ]
  for (const [path, method] of business) {
    for (const [kind, headers] of [
      ['Cookie', cookieHeaders], ['session Bearer', { Authorization: `Bearer ${cookie.value}` }], ['session X-API-Key', { 'X-API-Key': cookie.value }]
    ]) {
      const reply = await request(fixture.origin, path, {
        method, headers,
        ...(method === 'GET' ? {} : { body: { query: 'ci-only', urls: ['https://example.invalid/article'] } })
      })
      assert.equal(reply.status, 401, `${path}: explicit ${kind} must not authorize business calls`)
    }
  }
  const rpc = { jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'ci-no-such-tool', arguments: {} } }
  for (const path of ['/mcp', '/v1/mcp']) {
    for (const headers of [cookieHeaders, { Authorization: `Bearer ${cookie.value}` }]) {
      assert.equal((await request(fixture.origin, path, { method: 'POST', headers, body: rpc })).status, 401, 'MCP rejects session credentials')
    }
    const reply = await request(fixture.origin, path, { method: 'POST', headers: { Authorization: `Bearer ${key}` }, body: rpc })
    assert.equal(reply.status, 200, 'Key passes MCP auth without calling a provider')
    assert.equal(JSON.parse(reply.body).error.code, -32602, 'Synthetic unknown tool reaches method validation')
  }

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
  return { key, token }
}

async function holdResponses(fixture, page, paths, status) {
  const arrived = deferred()
  const released = deferred()
  const settled = deferred()
  const remaining = new Set(paths)
  let captured = 0
  let completed = 0
  let failure
  gates.add(released)
  const matcher = (url) => paths.includes(url.pathname)
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
    async wait() { await bounded(arrived.promise, 'real response capture'); if (failure) throw failure },
    async release() {
      released.resolve()
      await bounded(settled.promise, 'held response delivery')
      gates.delete(released)
      await page.unroute(matcher, handler)
      if (failure) throw failure
    }
  }
}

async function refresh(page) {
  await page.locator('.page-actions .el-button[title="\u5237\u65b0"]').click()
}

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
  const { key, token } = await authBoundaries(fixture, first)

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
  await removeFixture(fixture)
  console.log(`Packaged ${fixture.label} Cookie, shared-tab, CSRF, Key, removed-route and stale-response cases completed`)
}

async function cleanup() {
  if (cleanupPromise) return cleanupPromise
  cleanupPromise = (async () => {
    operations.abort()
    const failures = []
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
    browser = await chromium.launch({ headless: true })
    for (const mode of ['http', 'https']) {
      fixture = await startFixture(mode, tls)
      await exercise(fixture)
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

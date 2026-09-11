'use strict'

if (process.env.GITHUB_ACTIONS !== 'true') {
  throw new Error('Admin session browser checks may run only in GitHub Actions')
}

const assert = require('node:assert/strict')
const { spawn } = require('node:child_process')
const { mkdir } = require('node:fs/promises')
const { resolve } = require('node:path')
const { stripVTControlCharacters } = require('node:util')

const origin = 'http://127.0.0.1:4173'
const sessionA = 'synthetic-admin-session-A'
const sessionB = 'synthetic-admin-session-B'
const cookieName = 'searchmeld_admin_session'
const credentials = { username: 'fixture-operator', password: 'synthetic-password-not-persisted' }
const searchQuery = 'synthetic-search-not-replayed'
const tokenKeys = ['searchmeld-admin-token', 'one-search-admin-token']
const desktop = { width: 1440, height: 1000 }
const mobile = { width: 390, height: 844 }
const contexts = new Set()
/** Builds a Playwright JSON reply with an explicit HTTP status. */
const json = (body, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })
/** Uses the admin API error envelope so rejection assertions exercise body parsing. */
const failure = (status = 401, message = 'admin login required') => json({ error: { message, status } }, status)
/** Issues a synthetic Cookie without exposing a session token in the login JSON. */
const loginReply = (session) => ({
  ...json({ expires_at: '2099-01-01T00:00:00Z' }),
  issueSession: session,
  headers: { 'Set-Cookie': `${cookieName}=${session}; Path=/api/admin; HttpOnly; SameSite=Lax` }
})

/** Creates a manually released gate for deterministic request and lifecycle ordering. */
function deferred() {
  let resolve
  const promise = new Promise(/** Exposes fulfillment without a timer or a separate reject callback. */ (done) => { resolve = done })
  return { promise, resolve }
}

/** Limits the wait and clears its timer; it does not cancel the underlying operation. */
async function bounded(promise, label, milliseconds = 15000) {
  let timer
  try {
    return await Promise.race([
      promise,
      new Promise(/** Rejects with the operation label when the finite wait expires. */ (_, reject) => {
        timer = setTimeout(/** Leaves the stalled operation identifiable in CI output. */ () => reject(new Error(`Timed out: ${label}`)), milliseconds)
      })
    ])
  } finally {
    clearTimeout(timer)
  }
}

/** Separates deliberately induced API errors from unexpected page/network failures. */
function observeErrors(app, page) {
  page.on('pageerror', /** Accepts only messages explicitly expected by the active browser case. */ (error) => {
    if (app.expectedErrors.has(error.message)) app.diagnostics.push(error.message)
    else app.problems.push(`pageerror: ${error.message}`)
  })
  page.on('console', /** Correlates failed-resource messages with recorded mocked request failures. */ (message) => {
    if (message.type() !== 'error') return
    if (message.text().startsWith('Failed to load resource:') && app.requests.some((request) => request.failed && request.url === message.location().url)) {
      app.diagnostics.push(message.text())
    } else {
      app.problems.push(`console: ${message.text()}`)
    }
  })
}

/**
 * Opens an isolated synthetic Cookie jar, mocks API responses and rejects external
 * requests. Tracks the context immediately so partial setup can still be cleaned up.
 */
async function openApp(browser, { session = '', legacy = false, viewport = desktop } = {}) {
  const context = await browser.newContext({ viewport, serviceWorkers: 'block', reducedMotion: 'reduce' })
  contexts.add(context)
  context.setDefaultTimeout(15000)
  const page = await context.newPage()
  const app = { context, page, sessions: new Set(session ? [session] : []), requests: [], overrides: new Map(), problems: [], expectedErrors: new Set(), diagnostics: [] }
  if (session) await context.addCookies([{ name: cookieName, value: session, domain: '127.0.0.1', path: '/api/admin', httpOnly: true, sameSite: 'Lax' }])
  const provider = { id: 1, name: 'tavily', display_name: 'Tavily', base_url: 'https://fixture.invalid', enabled: true, priority: 1, weight: 1, timeout_ms: 1000, available_keys: 1 }
  const defaults = new Map(Object.entries({
    'GET /api/admin/providers': json({ providers: [provider] }),
    'GET /api/admin/keys': json({ keys: [] }),
    'GET /api/admin/tokens': json({ tokens: [] }),
    'GET /api/admin/request-logs': json({ logs: [] }),
    'GET /api/admin/logs': json({ logs: [] }),
    'GET /api/admin/audit-logs': json({ logs: [] }),
    'GET /api/admin/providers/health': json({ providers: [] }),
    'GET /api/admin/settings': json({ default_mode: 'fallback', default_providers: ['tavily'], default_limit: 5 }),
    'GET /api/admin/dashboard': json({
      usage: { requests_total: 0, requests_success: 0, requests_failed: 0, cache_hits: 0, results_total: 0, average_latency_ms: 0 },
      providers: [], provider_health: [], billing: { days: 14, units: [] }
    }),
    'GET /api/admin/me': json({ username: credentials.username }),
    'POST /api/admin/logout': json({ status: 'ok' })
  }))

  observeErrors(app, page)
  await context.routeWebSocket('**/*', /** Allows Vite's local socket while reporting unexpected external connections. */ async (socket) => {
    if (new URL(socket.url()).origin === origin.replace('http:', 'ws:')) socket.connectToServer()
    else {
      app.problems.push(`Unexpected external WebSocket: ${socket.url()}`)
      await socket.close()
    }
  })
  await context.route('**/*', /** Enforces browser credential transport and consumes one-shot synthetic API overrides. */ async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    try {
      assert.equal(url.origin, origin, `Unexpected external request: ${request.url()}`)
      if (url.pathname === '/__session_fixture__') {
        return await route.fulfill({ contentType: 'text/html', body: '<!doctype html><title>Session fixture</title>' })
      }
      if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/v1/') || url.pathname === '/healthz' || url.pathname === '/mcp') {
        const key = `${request.method()} ${url.pathname}`
        const headers = await request.allHeaders()
        const cookie = (headers.cookie || '').split(';').map((part) => part.trim()).find((part) => part.startsWith(`${cookieName}=`))?.slice(cookieName.length + 1) || ''
        const admin = url.pathname.startsWith('/api/admin/')
        assert.equal(headers.authorization, undefined, 'Browser attached Authorization')
        assert.equal(headers['x-api-key'], undefined, 'Browser attached an API key')
        assert.equal(headers['x-searchmeld-admin'], admin ? '1' : undefined)
        const handler = app.overrides.get(key) || (admin && !app.sessions.has(cookie) && url.pathname !== '/api/admin/login' ? failure() : defaults.get(key))
        assert.ok(handler, `Unexpected API request: ${key}`)
        const entry = { key, url: request.url(), cookie, body: request.postData(), failed: false }
        app.requests.push(entry)
        const reply = typeof handler === 'function' ? await handler(entry) : handler
        entry.failed = Boolean(reply.abort || reply.status >= 400)
        if (reply.abort) await route.abort('failed')
        else {
          const { issueSession, ...response } = reply
          if (issueSession) app.sessions.add(issueSession)
          if (admin && url.pathname !== '/api/admin/login' &&
            (reply.status === 401 || (url.pathname === '/api/admin/logout' && reply.status === 200))) app.sessions.delete(cookie)
          await route.fulfill(response)
        }
        return
      }
      assert.ok(request.method() === 'GET' && !['fetch', 'xhr'].includes(request.resourceType()), `Unexpected non-asset request: ${request.url()}`)
      await route.continue()
    } catch (error) {
      app.problems.push(error.message)
      await route.abort().catch(/** The recorded routing failure remains authoritative if abort also fails. */ () => {})
    }
  })

  // Seed once on an inert same-origin document, never from a reload-time init script.
  await page.goto(`${origin}/__session_fixture__`)
  await page.evaluate(/** Seeds legacy storage once so reload cannot silently repopulate cleared credentials. */ ({ session, legacy, tokenKeys }) => {
    if (legacy) {
      for (const key of tokenKeys) sessionStorage.setItem(key, session || 'synthetic-legacy-session')
      for (const key of tokenKeys) localStorage.setItem(key, 'synthetic-old-local-copy')
    }
  }, { session, legacy, tokenKeys })
  return app
}

/** Instruments the real store/router to count probes and recovery navigation without replacing auth logic. */
async function observe(app) {
  await app.page.evaluate(/** Installs per-document observation after each full load or reload. */ async () => {
    const [{ default: router }, { useSessionStore }, { apiFetch }] = await Promise.all([
      import('/src/router/index.ts'), import('/src/stores/session.ts'), import('/src/api/client.ts')
    ])
    window.authProbe = { router, session: useSessionStore(), apiFetch, transitions: [], replacements: [], checkCalls: 0 }
    window.authProbe.session.$onAction(/** Counts probe attempts, including calls sharing one pending request. */ ({ name }) => { if (name === 'check') window.authProbe.checkCalls += 1 })
    router.afterEach(/** Counts only completed navigation, not canceled or superseded guards. */ (to, _from, failure) => { if (!failure) window.authProbe.transitions.push(to.fullPath) })
    const replace = router.replace.bind(router)
    /** Records replacement targets while preserving the real router's returned promise. */
    router.replace = (to) => {
      window.authProbe.replacements.push(router.resolve(to).fullPath)
      return replace(to)
    }
  })
}

/** Awaits both URLs and disables Logs polling; paused-clock cases avoid RAF-dependent router waits. */
async function at(app, path) {
  const expected = new URL(path, origin).href
  await app.page.waitForURL(expected)
  await app.page.waitForFunction(/** Waits for the router state as well as the browser URL before asserting a destination. */ (expected) => new URL(window.authProbe.router.currentRoute.value.fullPath, location.origin).href === expected, expected, { polling: app.logClockPaused ? 50 : 'raf' })
  if (new URL(path, origin).pathname === '/logs') {
    // Element Plus puts the switch role on a zero-sized input; click its visible wrapper.
    const toggle = app.page.locator('.logs-actions .el-switch')
    await toggle.waitFor()
    if (await toggle.locator('[role="switch"]').getAttribute('aria-checked') === 'true') await toggle.click()
    await toggle.locator('[role="switch"][aria-checked="false"]').waitFor({ state: 'attached' })
  }
}

/** Loads a fresh document and reinstalls observation of its real session/router modules. */
async function visit(app, path) {
  await app.page.goto(`${origin}${path}`)
  await observe(app)
}

/** Performs client-side navigation and waits for the canonical internal destination. */
async function navigate(app, path) {
  await app.page.evaluate((path) => window.authProbe.router.push(path), path)
  await at(app, path)
}

/** Holds exactly one matching API reply after recording arrival; later requests use normal routing. */
function hold(app, key, reply) {
  const seen = deferred()
  const release = deferred()
  app.overrides.set(key, /** Removes the override before waiting so concurrent follow-up requests are not also held. */ async (request) => {
    app.overrides.delete(key)
    seen.resolve(request)
    await release.promise
    return reply
  })
  return { seen: seen.promise, release: release.resolve }
}

/** Asserts legacy credentials and synthetic form/request secrets are absent from both storage areas. */
async function storageClean(app) {
  const state = await app.page.evaluate(/** Snapshots storage and checks that the transient store has no token API. */ () => ({
    hasToken: 'token' in window.authProbe.session || 'setToken' in window.authProbe.session,
    session: { ...sessionStorage }, local: { ...localStorage }
  }))
  assert.equal(state.hasToken, false)
  for (const key of tokenKeys) {
    assert.equal(state.session[key], undefined)
    assert.equal(state.local[key], undefined)
  }
  for (const secret of [credentials.username, credentials.password, searchQuery]) {
    assert.ok(!JSON.stringify([state.session, state.local]).includes(secret), 'Form/request data was persisted')
  }
}

/** Verifies storage cleanup and the synthetic jar's expected Cookie value and transport attributes. */
async function sessionIs(app, session) {
  await storageClean(app)
  const cookie = (await app.context.cookies(`${origin}/api/admin/me`)).find((cookie) => cookie.name === cookieName)
  assert.equal(cookie?.value, session || undefined)
  if (cookie) {
    assert.equal(cookie.httpOnly, true)
    assert.equal(cookie.path, '/api/admin')
    assert.equal(cookie.sameSite, 'Lax')
  }
}

/** Reintroduces obsolete stored tokens so subsequent auth operations must clear them again. */
async function legacyCopies(app) {
  await app.page.evaluate(/** Seeds both legacy key names without modifying the Cookie jar. */ ({ keys, session }) => {
    for (const key of keys) sessionStorage.setItem(key, session)
    for (const key of keys) localStorage.setItem(key, session)
  }, { keys: tokenKeys, session: sessionA })
}

/** Checks anonymous login state, exact return target and viewport-bounded layout without hidden overflow. */
async function onLogin(app, redirect) {
  await app.page.locator('.login-card').waitFor()
  const viewport = app.page.viewportSize()
  assert.ok(viewport, 'Login layout requires an explicit browser viewport')
  const layout = await app.page.evaluate(/** Measures both scroll widths and element bounds instead of relying on screenshot dimensions alone. */ () => ({
    viewport: document.documentElement.clientWidth,
    windowWidth: window.innerWidth,
    elements: ['html', 'body', '#app', '.login-page', '.login-card'].map(/** Requires every layout boundary used by the login overflow regression. */ (selector) => {
      const element = document.querySelector(selector)
      if (!element) throw new Error(`Missing login layout element: ${selector}`)
      const bounds = element.getBoundingClientRect()
      return { selector, left: bounds.left, right: bounds.right, clientWidth: element.clientWidth, scrollWidth: element.scrollWidth }
    })
  }))
  assert.equal(layout.windowWidth, viewport.width, 'Login window must retain the configured viewport width')
  assert.ok(layout.viewport <= viewport.width, 'Login root must not expand the configured viewport')
  for (const element of layout.elements) {
    assert.ok(element.scrollWidth <= element.clientWidth + 1, `${element.selector} scrollWidth ${element.scrollWidth} exceeds clientWidth ${element.clientWidth}`)
    assert.ok(element.left >= -1 && element.right <= viewport.width + 1, `${element.selector} bounds [${element.left}, ${element.right}] exceed configured viewport ${viewport.width}`)
  }
  const url = new URL(app.page.url())
  assert.equal(url.pathname, '/login')
  assert.equal(url.searchParams.get('redirect'), redirect)
  await storageClean(app)
  assert.equal(await app.page.evaluate(() => window.authProbe.session.profile), null)
}

/** Submits synthetic credentials once, checks pending-state deduplication and verifies the issued Cookie. */
async function login(app, target, session = sessionB) {
  const attempts = app.requests.filter((request) => request.key === 'POST /api/admin/login').length
  const response = hold(app, 'POST /api/admin/login', loginReply(session))
  const inputs = app.page.locator('.login-card input')
  await inputs.nth(0).fill(credentials.username)
  await inputs.nth(1).fill(credentials.password)
  const button = app.page.locator('.login-card .el-button')
  await button.click()
  const request = await response.seen
  assert.deepEqual(JSON.parse(request.body), credentials)
  assert.equal(await button.isDisabled(), true)
  await app.page.locator('.login-card form').dispatchEvent('submit')
  response.release()
  await at(app, target)
  await sessionIs(app, session)
  assert.equal(app.requests.filter((request) => request.key === 'POST /api/admin/login').length, attempts + 1)
}

/** Starts real apiFetch calls without waiting for held responses or replaying failed requests. */
async function startRequests(app, requests) {
  await app.page.evaluate(/** Keeps every settlement observable while allowing concurrent recovery. */ (requests) => {
    window.authProbe.pending = Promise.allSettled(requests.map(({ path, options }) => window.authProbe.apiFetch(path, options)))
  }, requests.map((request) => typeof request === 'string' ? { path: request } : request))
}

/** Requires each pending API request to preserve its expected rejection message within a finite wait. */
async function rejected(app, messages) {
  const results = await bounded(app.page.evaluate(/** Awaits all original requests so late recovery cannot escape the rejection assertions. */ async () => (await window.authProbe.pending).map((result) => ({
    status: result.status, message: result.status === 'rejected' ? result.reason.message : undefined
  }))), 'request rejection')
  assert.deepEqual(results, messages.map((message) => ({ status: 'rejected', message })))
}

/** Proves concurrent expiration caused one replacement and one completed login navigation. */
async function oneExpiry(app) {
  const counts = await app.page.evaluate(/** Counts navigation effects independently of HTTP request counts. */ () => ({
    replacements: window.authProbe.replacements.length,
    logins: window.authProbe.transitions.filter((path) => path.startsWith('/login?')).length
  }))
  assert.deepEqual(counts, { replacements: 1, logins: 1 })
}

/** Writes a synthetic full-page PNG only when Actions supplies an artifact directory. */
async function screenshot(app, name) {
  if (!process.env.ARTIFACT_DIR) return
  await mkdir(process.env.ARTIFACT_DIR, { recursive: true })
  await app.page.screenshot({ path: resolve(process.env.ARTIFACT_DIR, `${name}.png`), fullPage: true })
}

/** Closes a completed case, releases its tracked context and fails on unexpected browser diagnostics. */
async function closeApp(app) {
  await bounded(app.context.close(), 'context cleanup', 5000)
  contexts.delete(app.context)
  assert.deepEqual(app.problems, [], 'Unexpected browser/network errors')
  console.log(`Browser case completed (${app.diagnostics.length} intentional error diagnostics)`)
}

/** Expires the default user-request refresh on each viewport without replay, preserving return URL and history. */
async function roundTrip(browser, viewport, name) {
  const app = await openApp(browser, { viewport })
  const target = '/logs?probe=a%2Bb#row-7'
  await visit(app, target)
  await onLogin(app, target)
  await app.page.reload()
  await observe(app)
  await onLogin(app, target)
  await login(app, target, sessionA)
  await navigate(app, '/tokens')
  await navigate(app, target)
  await app.page.locator('.logs-actions button[title="\u5237\u65b0"]:not(.is-loading)').waitFor()
  await app.page.evaluate(/** Excludes setup navigation from the single-expiry recovery counts. */ () => { window.authProbe.replacements = []; window.authProbe.transitions = [] })
  await legacyCopies(app)
  app.expectedErrors.add('admin login required')
  const refresh = hold(app, 'GET /api/admin/request-logs', failure())
  await app.page.locator('.logs-actions button[title="\u5237\u65b0"]').click()
  assert.equal((await refresh.seen).cookie, sessionA)
  refresh.release()
  await onLogin(app, target)
  await oneExpiry(app)
  await app.page.waitForFunction(() => document.querySelector('.login-logo')?.naturalWidth > 0)
  await screenshot(app, `${name}-expired`)
  await login(app, target)
  assert.equal(app.requests.filter((request) => request.key === 'GET /api/admin/request-logs' && request.cookie === sessionA).length, 3)
  await app.page.locator('.logs-actions button[title="\u5237\u65b0"]:not(.is-loading)').waitFor()
  await screenshot(app, `${name}-returned`)
  await app.page.goBack()
  await at(app, '/tokens')
  await sessionIs(app, sessionB)
  await closeApp(app)
}

/** Requires initial protected-request 401 recovery for JSON, empty and HTML response bodies without replay. */
async function entryExpiry(browser) {
  for (const reply of [failure(), { status: 401, body: '' }, { status: 401, contentType: 'text/html', body: '<h1>Unauthorized</h1>' }]) {
    const app = await openApp(browser, { session: sessionA })
    app.expectedErrors.add('admin login required').add('Unauthorized')
    const response = hold(app, 'GET /api/admin/tokens', reply)
    const target = '/tokens?probe=entry#list'
    await visit(app, target)
    assert.equal((await response.seen).cookie, sessionA)
    await legacyCopies(app)
    response.release()
    await onLogin(app, target)
    await oneExpiry(app)
    await app.page.reload()
    await observe(app)
    await onLogin(app, target)
    await login(app, target)
    await app.page.locator('.token-table').waitFor()
    assert.deepEqual(app.requests.filter((request) => request.key === 'GET /api/admin/tokens').map((request) => request.cookie), [sessionA, sessionB])
    await closeApp(app)
  }
}

/** Checks simultaneous and staggered 401 replies share one recovery navigation without replaying requests. */
async function parallelExpiry(browser) {
  for (const staggered of [false, true]) {
    const app = await openApp(browser, { session: sessionA })
    const target = '/providers?probe=parallel#keys'
    await visit(app, target)
    await app.page.locator('.provider-grid').waitFor()
    const providers = hold(app, 'GET /api/admin/providers', failure())
    const keys = hold(app, 'GET /api/admin/keys', failure())
    await startRequests(app, ['/api/admin/providers', '/api/admin/keys'])
    const requests = await Promise.all([providers.seen, keys.seen])
    assert.deepEqual(requests.map((request) => request.cookie), [sessionA, sessionA])
    providers.release()
    if (staggered) await onLogin(app, target)
    keys.release()
    await rejected(app, ['admin login required', 'admin login required'])
    await onLogin(app, target)
    await oneExpiry(app)
    await login(app, target)
    await app.page.locator('.provider-grid').waitFor()
    for (const key of ['GET /api/admin/providers', 'GET /api/admin/keys']) {
      assert.equal(app.requests.filter((request) => request.key === key && request.cookie === sessionA).length, 2)
    }
    await closeApp(app)
  }
}

/** Keeps newer login intact after an old request fails, while still expiring a later current-session request. */
async function lateResponse(browser) {
  const app = await openApp(browser, { session: sessionA })
  await visit(app, '/providers')
  await app.page.locator('.provider-grid').waitFor()
  const late = hold(app, 'GET /api/admin/keys', failure())
  const expire = hold(app, 'GET /api/admin/logs', failure())
  await startRequests(app, ['/api/admin/keys?probe=late', '/api/admin/logs?probe=expire'])
  await Promise.all([late.seen, expire.seen])
  const current = '/tokens?probe=current#edit'
  await navigate(app, current)
  expire.release()
  await onLogin(app, current)
  await oneExpiry(app)
  await login(app, current)
  late.release()
  await rejected(app, ['admin login required', 'admin login required'])
  await at(app, current)
  await sessionIs(app, sessionB)
  assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 2)
  const expireB = hold(app, 'GET /api/admin/keys', failure())
  await startRequests(app, ['/api/admin/keys?probe=current-B'])
  assert.equal((await expireB.seen).cookie, sessionB)
  expireB.release()
  await rejected(app, ['admin login required'])
  await onLogin(app, current)
  assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 3)
  await closeApp(app)
}

/** Proves recovery starts before error-body parsing and a released old body cannot clear newer auth state. */
async function bodyRace(browser) {
  const app = await openApp(browser, { session: sessionA })
  await visit(app, '/tokens')
  await app.page.locator('.token-table').waitFor()
  app.overrides.set('GET /api/admin/logs', failure())
  // Split the response headers/body await boundary without replacing apiFetch's auth logic.
  await app.page.evaluate(/** Delays only JSON consumption for the marked response while keeping the real fetch path. */ () => {
    const fetch = window.fetch
    /** Restores fetch after installing the one-shot response-body gate. */
    window.fetch = async (...args) => {
      const response = await fetch(...args)
      if (String(args[0]).includes('probe=body-race')) {
        window.fetch = fetch
        const read = response.json.bind(response)
        /** Exposes a release gate after headers have already triggered apiFetch recovery. */
        response.json = async () => {
          window.authProbe.bodyStarted = true
          await new Promise(/** Holds body parsing until the newer login has completed. */ (resolve) => { window.authProbe.releaseBody = resolve })
          return read()
        }
      }
      return response
    }
  })
  await startRequests(app, ['/api/admin/logs?probe=body-race'])
  await onLogin(app, '/tokens')
  await app.page.waitForFunction(() => window.authProbe.bodyStarted)
  await oneExpiry(app)
  await login(app, '/tokens')
  await app.page.evaluate(() => window.authProbe.releaseBody())
  await rejected(app, ['admin login required'])
  await sessionIs(app, sessionB)
  await at(app, '/tokens')
  assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 2)
  await closeApp(app)
}

/** Treats login 401 as a form error, clears the password on reload and permits one later successful retry. */
async function wrongPassword(browser) {
  const app = await openApp(browser)
  const target = '/tokens?probe=password#retry'
  await visit(app, target)
  const badLogin = hold(app, 'POST /api/admin/login', failure(401, 'invalid username or password'))
  await app.page.locator('.login-card input').nth(0).fill(credentials.username)
  await app.page.locator('.login-card input').nth(1).fill(credentials.password)
  await app.page.locator('.login-card .el-button').click()
  assert.equal((await badLogin.seen).cookie, '')
  badLogin.release()
  await app.page.getByText('invalid username or password', { exact: true }).waitFor()
  await app.page.locator('.login-card .el-button:not(.is-loading)').waitFor()
  await onLogin(app, target)
  assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 0)
  await app.page.reload()
  await observe(app)
  assert.equal(await app.page.locator('.login-card input').nth(1).inputValue(), '')
  await login(app, target)
  assert.equal(app.requests.filter((request) => request.key === 'POST /api/admin/login').length, 2)
  await closeApp(app)
}

/** Preserves auth across legacy/new log-read failures and proves failed search writes are never replayed. */
async function otherErrors(browser) {
  const app = await openApp(browser, { session: sessionA })
  const target = '/playground?probe=errors#search'
  await visit(app, target)
  await app.page.locator('.search-input input').waitFor()
  const cases = [
    ...[403, 429, 500].map((status) => ({ path: '/api/admin/logs', reply: failure(status, `fixture-${status}`), message: `fixture-${status}` })),
    { path: '/api/admin/logs', reply: { abort: true }, message: 'Failed to fetch' },
    ...['/api/admin/request-logs', '/api/admin/request-logs/41'].flatMap((path) => [
      ...[403, 429, 500].map((status) => ({ path, reply: failure(status, `entry-${status}`), message: `entry-${status}` })),
      { path, reply: { abort: true }, message: 'Failed to fetch' }
    ]),
    { path: '/v1/search', reply: failure(401, 'API token rejected'), message: 'API token rejected', options: { method: 'POST', body: JSON.stringify({ query: searchQuery }) } },
    { path: '/api/admin/login?probe=existing-session', reply: failure(401, 'credentials rejected'), message: 'credentials rejected', options: { method: 'POST', body: JSON.stringify(credentials) } },
    { path: '/api/admin/me', reply: failure(403, 'proof rejected'), message: 'proof rejected' }
  ]
  for (const test of cases) {
    const response = hold(app, `${test.options?.method || 'GET'} ${new URL(test.path, origin).pathname}`, test.reply)
    await startRequests(app, [test])
    assert.equal((await response.seen).cookie, test.path.startsWith('/api/admin/') ? sessionA : '')
    response.release()
    await rejected(app, [test.message])
    await sessionIs(app, sessionA)
    await at(app, target)
    assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 0)
  }

  for (const status of [500, 401]) {
    const search = hold(app, 'POST /api/admin/playground/search', failure(status, `search-${status}`))
    await app.page.locator('.search-input input').fill(searchQuery)
    await app.page.locator('.search-btn').click()
    assert.equal(JSON.parse((await search.seen).body).query, searchQuery)
    assert.equal(await app.page.locator('.search-btn').isDisabled(), true)
    search.release()
    await app.page.getByText(`search-${status}`, { exact: true }).waitFor()
    if (status === 500) {
      await app.page.locator('.search-btn:not(.is-loading)').waitFor()
      await sessionIs(app, sessionA)
    } else {
      await onLogin(app, target)
      await oneExpiry(app)
      await login(app, target)
      await app.page.locator('.search-input input').waitFor()
      assert.equal(await app.page.locator('.search-input input').inputValue(), '')
    }
  }
  assert.equal(app.requests.filter((request) => request.key === 'POST /api/admin/playground/search').length, 2)
  await app.context.clearCookies()
  await app.page.evaluate(() => window.authProbe.router.replace('/login'))
  const count = await app.page.evaluate(() => window.authProbe.replacements.length)
  const empty = hold(app, 'GET /api/admin/providers', failure())
  await startRequests(app, ['/api/admin/providers'])
  assert.equal((await empty.seen).cookie, '')
  empty.release()
  await rejected(app, ['admin login required'])
  await onLogin(app, null)
  assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), count)
  await closeApp(app)
}

/** Distinguishes confirmed logout from retryable failures while retaining late-request and legacy-storage checks. */
async function manualLogout(browser) {
  for (const status of [200, 401, 403, 500, 'network']) {
    const app = await openApp(browser, { session: sessionA, legacy: true })
    await visit(app, '/providers')
    await app.page.locator('.provider-grid').waitFor()
    await sessionIs(app, sessionA)
    const late = hold(app, 'GET /api/admin/keys', failure())
    await startRequests(app, ['/api/admin/keys?probe=after-logout'])
    await late.seen
    await legacyCopies(app)
    const reply = status === 'network' ? { abort: true } : status === 200 ? json({ status: 'ok' }) : failure(status, 'logout rejected')
    const logout = hold(app, 'POST /api/admin/logout', reply)
    await app.page.locator('.float-logout').click()
    assert.equal((await logout.seen).cookie, sessionA)
    assert.equal(await app.page.locator('.float-logout').isDisabled(), true)
    logout.release()
    if (status === 200 || status === 401) {
      await onLogin(app, null)
    } else {
      await app.page.locator('.logout-error').getByText(status === 'network' ? 'Failed to fetch' : 'logout rejected', { exact: true }).waitFor()
      await at(app, '/providers')
      assert.equal(await app.page.locator('.login-card').count(), 0)
      assert.equal(app.sessions.has(sessionA), true)
      assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 0)
    }
    await sessionIs(app, sessionA)
    late.release()
    await rejected(app, ['admin login required'])
    if (status !== 200 && status !== 401) {
      await app.page.locator('.logout-error .el-button').click()
    }
    await onLogin(app, null)
    assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 1)
    await app.page.reload()
    await observe(app)
    await onLogin(app, null)
    if (status === 200) {
      await app.page.evaluate(() => window.authProbe.router.push('/tokens?probe=guard#later'))
      await onLogin(app, '/tokens?probe=guard#later')
      await app.page.evaluate(() => window.authProbe.router.replace('/login'))
    }
    await login(app, '/playground')
    assert.equal(app.requests.filter((request) => request.key === 'POST /api/admin/logout').length, status === 200 || status === 401 ? 1 : 2)
    assert.equal(app.requests.filter((request) => request.url.includes('probe=after-logout')).length, 1)
    await closeApp(app)
  }
}

/** Waits for a held logout to settle after expiry or reauthentication and verifies only current navigation survives. */
async function logoutAcrossExpiry(browser) {
  for (const status of [200, 401, 'network']) {
    for (const reauthenticate of [false, true]) {
      const app = await openApp(browser, { session: sessionA })
      const target = '/tokens?probe=pending-logout#list'
      await visit(app, target)
      await app.page.locator('.token-table').waitFor()
      await app.page.evaluate(/** Captures the original logout promise so assertions cannot race its late callbacks. */ async () => {
        const { api } = await import('/src/api/client.ts')
        const logout = api.logout
        /** Instruments one invocation and returns the unmodified API promise to App. */
        api.logout = () => {
          window.authProbe.logoutPending = logout()
          api.logout = logout
          return window.authProbe.logoutPending
        }
      })

      const reply = status === 'network' ? { abort: true } : status === 200 ? json({ status: 'ok' }) : failure(status, 'logout rejected')
      const logout = hold(app, 'POST /api/admin/logout', reply)
      await app.page.locator('.float-logout').click()
      assert.equal((await logout.seen).cookie, sessionA)
      const expire = hold(app, 'GET /api/admin/keys', failure())
      await startRequests(app, ['/api/admin/keys?probe=logout-expiry'])
      assert.equal((await expire.seen).cookie, sessionA)
      expire.release()
      await rejected(app, ['admin login required'])
      await onLogin(app, target)
      await oneExpiry(app)
      if (reauthenticate) {
        await login(app, target)
        await app.page.locator('.token-table').waitFor()
      }
      const transitions = await app.page.evaluate(() => window.authProbe.transitions.slice())

      logout.release()
      // App registered its await first; the unchanged API promise includes response-body parsing.
      const message = await bounded(app.page.evaluate(/** Waits through body parsing before checking whether newer auth was disturbed. */ async () => {
        if (!window.authProbe.logoutPending) throw new Error('No pending logout was recorded')
        try {
          await window.authProbe.logoutPending
          return null
        } catch (error) {
          return error.message
        }
      }), 'pending logout settlement')
      assert.equal(message, status === 200 ? null : status === 'network' ? 'Failed to fetch' : 'logout rejected')
      if (reauthenticate) {
        await sessionIs(app, sessionB)
        await at(app, target)
        assert.deepEqual(await app.page.evaluate(() => window.authProbe.transitions), transitions)
      } else if (status !== 'network') {
        await at(app, '/login')
        await onLogin(app, null)
        assert.deepEqual(await app.page.evaluate(() => window.authProbe.transitions), [...transitions, '/login'])
      } else {
        await onLogin(app, target)
        await app.page.locator('.logout-error').getByText('Failed to fetch', { exact: true }).waitFor()
        assert.deepEqual(await app.page.evaluate(() => window.authProbe.transitions), transitions)
      }
      assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), reauthenticate || status !== 'network' ? 2 : 1)
      assert.equal(app.requests.filter((request) => request.key === 'POST /api/admin/logout').length, 1)
      assert.equal(app.requests.filter((request) => request.url.includes('probe=logout-expiry')).length, 1)
      await closeApp(app)
    }
  }
}

/** Checks form submission and authenticated-login guards agree on safe destinations and invalid-target fallback. */
async function returnTargets(browser) {
  const app = await openApp(browser)
  await visit(app, '/login')
  const targets = [
    ['/tokens?probe=a%2Bb#edit', '/tokens?probe=a%2Bb#edit'],
    ['/logs?label=\u63d0\u53d6#\u8be6\u60c5', '/logs?label=\u63d0\u53d6#\u8be6\u60c5'],
    ['/keys?probe=alias#keys', '/providers?probe=alias#keys'],
    ['/usage?probe=alias#chart', '/?probe=alias#chart'],
    ...[
      undefined, null, '', ['/tokens', '/providers'], 'tokens', ' /tokens',
      'https://example.invalid/tokens', `${origin}/tokens`, '//example.invalid/tokens',
      '/\\example.invalid', '/%2f%2fexample.invalid', '/%5cexample.invalid', '/tokens%2f',
      'javascript:alert(1)', 'data:text/html,invalid', '/unknown', '/login',
      '/login?redirect=/tokens', '/LOGIN/', '/login/', '/providers/../tokens',
      '/tokens?bad=%', '/tokens?bad=%ZZ', '/tokens?bad=%E0%A4%A', '/tokens\n', '/tokens?bad=%00'
    ].map((target) => [target, '/playground'])
  ]
  for (const [redirect, expected] of targets) {
    await app.context.clearCookies()
    await app.page.evaluate(/** Passes raw invalid and valid query values through the actual login guard. */ (redirect) => window.authProbe.router.replace({ path: '/login', query: { redirect } }), redirect)
    await app.page.locator('.login-card').waitFor()
    await login(app, expected)
    // The authenticated-login guard must use the same validation as form submission.
    await app.page.evaluate(/** Tests the same raw return target with an already authenticated Cookie. */ (redirect) => window.authProbe.router.push({ path: '/login', query: { redirect } }), redirect)
    await at(app, expected)
    await sessionIs(app, sessionB)
  }
  await closeApp(app)
}

/** Opens an independent page sharing the existing jar, request fixtures and diagnostic collection. */
async function newTab(app, path) {
  const page = await app.context.newPage()
  observeErrors(app, page)
  const tab = { ...app, page }
  await visit(tab, path)
  return tab
}

/** Requires fresh route probes across tabs/reloads and proves legacy storage cannot replace a deleted Cookie. */
async function freshTabs(browser) {
  const target = '/tokens?probe=shared#list'
  const app = await openApp(browser, { legacy: true })
  await visit(app, target)
  await onLogin(app, target)
  await login(app, target, sessionA)
  /** Counts actual /me requests rather than store calls that may share a pending probe. */
  const checks = () => app.requests.filter((request) => request.key === 'GET /api/admin/me').length
  let count = checks()
  const tab = await newTab(app, '/providers')
  await at(tab, '/providers')
  await tab.page.locator('.provider-grid').waitFor()
  assert.equal(checks(), count + 1)
  assert.equal(await tab.page.evaluate(() => window.opener), null)
  await sessionIs(tab, sessionA)
  count = checks()
  await app.page.reload()
  await observe(app)
  await at(app, target)
  assert.equal(checks(), count + 1)
  count = checks()
  await navigate(app, '/logs')
  await navigate(app, target)
  assert.equal(checks(), count + 2, 'Settled /me success was reused as route permission')
  await app.page.evaluate(() => window.authProbe.router.push('/unknown'))
  await at(app, '/playground')
  await navigate(app, target)

  const isolated = await openApp(browser, { legacy: true })
  await visit(isolated, target)
  await onLogin(isolated, target)
  await sessionIs(isolated, '')
  assert.equal(isolated.requests.some((request) => request.key === 'GET /api/admin/tokens'), false)
  await closeApp(isolated)

  await legacyCopies(app)
  await legacyCopies(tab)
  await app.context.clearCookies()
  await startRequests(app, ['/api/admin/tokens?probe=deleted-cookie'])
  await startRequests(tab, ['/api/admin/providers?probe=deleted-cookie'])
  await rejected(app, ['admin login required'])
  await rejected(tab, ['admin login required'])
  await onLogin(app, target)
  await onLogin(tab, '/providers')
  await sessionIs(app, '')
  await sessionIs(tab, '')
  assert.equal(app.requests.filter((request) => request.url.includes('probe=deleted-cookie')).length, 2)
  await login(app, target)
  await tab.page.evaluate(() => window.authProbe.router.push('/providers'))
  await at(tab, '/providers')
  await sessionIs(tab, sessionB)
  assert.equal(await tab.page.locator('.login-card').count(), 0)
  await closeApp(app)
}

/** Keeps invalid or failed probes in retryable unknown state at bootstrap and during desktop/mobile navigation. */
async function probeErrors(browser) {
  const replies = [
    ...[403, 429, 500].map((status) => failure(status, `probe-${status}`)),
    { abort: true },
    { status: 200, contentType: 'text/html', body: '<h1>Not session metadata</h1>' },
    ...[null, [], {}, { username: '' }, { username: 1 }].map((body) => json(body))
  ]
  for (const path of ['/tokens?probe=bootstrap#list', '/login?redirect=%2Ftokens']) {
    for (const reply of replies) {
      const app = await openApp(browser, { session: path.startsWith('/login') ? '' : sessionA, legacy: true })
      const probe = hold(app, 'GET /api/admin/me', reply)
      await visit(app, path)
      await probe.seen
      await app.page.locator('.session-gate [role="status"]').waitFor()
      assert.equal(await app.page.locator('.login-card, .app-shell').count(), 0)
      probe.release()
      await app.page.locator('.session-gate [role="alert"]').waitFor()
      assert.equal(await app.page.locator('.login-card, .app-shell').count(), 0)
      assert.equal(app.requests.filter((request) => request.key !== 'GET /api/admin/me').length, 0)
      assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 0)
      await storageClean(app)
      if (reply.status === 500) await screenshot(app, 'desktop-probe-error')
      await app.page.locator('.session-gate .el-button').click()
      if (path.startsWith('/login')) await onLogin(app, '/tokens')
      else await at(app, path)
      await closeApp(app)
    }
  }
  for (const viewport of [desktop, mobile]) {
    const app = await openApp(browser, { session: sessionA, viewport })
    await visit(app, '/tokens')
    await app.page.locator('.token-table').waitFor()
    const probe = hold(app, 'GET /api/admin/me', failure(503, 'probe-unavailable'))
    await app.page.evaluate(/** Starts navigation without awaiting the deliberately held route-entry probe. */ () => { window.authProbe.navigation = window.authProbe.router.push('/providers?probe=retry#keys') })
    await probe.seen
    probe.release()
    await app.page.evaluate(/** Awaits the failed guard before asserting that the previous page stayed mounted. */ () => window.authProbe.navigation)
    await at(app, '/tokens')
    await app.page.locator('.session-error').getByText('probe-unavailable', { exact: true }).waitFor()
    assert.equal(await app.page.locator('.token-table').count(), 1)
    assert.equal(await app.page.locator('.provider-grid, .login-card').count(), 0)
    await sessionIs(app, sessionA)
    assert.deepEqual(await app.page.evaluate(() => window.authProbe.session.profile), { username: credentials.username })
    await screenshot(app, viewport === mobile ? 'mobile-probe-error' : 'desktop-navigation-error')
    await app.page.locator('.session-error .el-button').click()
    await at(app, '/providers?probe=retry#keys')
    await app.page.locator('.provider-grid').waitFor()
    await closeApp(app)
  }
}

/** Checks only pending same-revision probes are reused and stale probe results cannot overwrite newer login. */
async function pendingProbes(browser) {
  const app = await openApp(browser, { session: sessionA })
  await visit(app, '/tokens')
  await app.page.locator('.token-table').waitFor()
  const count = app.requests.filter((request) => request.key === 'GET /api/admin/me').length
  const checkCalls = await app.page.evaluate(() => window.authProbe.checkCalls)
  const pending = hold(app, 'GET /api/admin/me', json({ username: credentials.username }))
  await app.page.evaluate(/** Starts the first guard whose current-session probe will remain pending. */ () => { window.authProbe.firstNavigation = window.authProbe.router.push('/providers') })
  await pending.seen
  await app.page.evaluate(/** Supersedes the first destination while its same-revision probe is still reusable. */ () => { window.authProbe.secondNavigation = window.authProbe.router.push('/logs') })
  await app.page.waitForFunction(/** Requires both guards to request a check before releasing their shared HTTP probe. */ (minimum) => window.authProbe.checkCalls >= minimum, checkCalls + 2)
  assert.equal(app.requests.filter((request) => request.key === 'GET /api/admin/me').length, count + 1)
  pending.release()
  await app.page.evaluate(/** Settles both guards before checking that only the newer destination mounted. */ () => Promise.all([window.authProbe.firstNavigation, window.authProbe.secondNavigation]))
  await at(app, '/logs')
  assert.equal(app.requests.some((request) => request.key === 'GET /api/admin/providers'), false)
  await navigate(app, '/tokens')
  assert.equal(app.requests.filter((request) => request.key === 'GET /api/admin/me').length, count + 2)
  await closeApp(app)

  for (const reply of [failure(), failure(500, 'stale-probe-error'), json({ username: 'stale-profile' })]) {
    const app = await openApp(browser, { session: sessionA })
    await visit(app, '/tokens')
    await app.page.locator('.token-table').waitFor()
    const old = hold(app, 'GET /api/admin/me', reply)
    await app.page.evaluate(/** Captures the guard that will become stale across an auth revision change. */ () => { window.authProbe.oldNavigation = window.authProbe.router.push('/providers') })
    await old.seen
    app.overrides.set('POST /api/admin/login', loginReply(sessionB))
    // Exercise the same in-tab auth-operation boundary while the old guard is pending.
    await app.page.evaluate(/** Advances the same in-tab revision boundaries as real login while an old guard is pending. */ async (credentials) => {
      const { api } = await import('/src/api/client.ts')
      window.authProbe.session.advanceRevision()
      await api.login(credentials.username, credentials.password)
      window.authProbe.session.advanceRevision()
      await window.authProbe.router.replace('/tokens?probe=new-session')
    }, credentials)
    await at(app, '/tokens?probe=new-session')
    old.release()
    await app.page.evaluate(/** Settles the stale guard before checking the new session and destination survived. */ () => window.authProbe.oldNavigation)
    await at(app, '/tokens?probe=new-session')
    await sessionIs(app, sessionB)
    assert.equal(await app.page.locator('.session-error, .login-card').count(), 0)
    assert.deepEqual(await app.page.evaluate(() => window.authProbe.session.profile), { username: credentials.username })
    await closeApp(app)
  }
}

/** Preserves the original 401 while an unavailable recovery probe leaves a retryable gate without request replay. */
async function expiryProbeError(browser) {
  const app = await openApp(browser, { session: sessionA })
  const target = '/tokens?probe=expiry-check#list'
  await visit(app, target)
  await app.page.locator('.token-table').waitFor()
  const probe = hold(app, 'GET /api/admin/me', failure(503, 'expiry-probe-unavailable'))
  app.overrides.set('GET /api/admin/logs', failure())
  await startRequests(app, ['/api/admin/logs?probe=expiry-check'])
  await probe.seen
  probe.release()
  await rejected(app, ['admin login required'])
  await at(app, target)
  await app.page.locator('.session-error').getByText('expiry-probe-unavailable', { exact: true }).waitFor()
  assert.equal(await app.page.locator('.login-card').count(), 0)
  await sessionIs(app, sessionA)
  await app.page.locator('.session-error .el-button').click()
  await onLogin(app, target)
  assert.equal(app.requests.filter((request) => request.url.includes('/logs?probe=expiry-check')).length, 1)
  await closeApp(app)
}

/** Ensures a held old request/logout cannot erase another tab's newer Cookie or briefly expose a password form. */
async function crossTabLateResponses(browser) {
  for (const kind of ['request', 'logout', 'logout-401', 'logout-network']) {
    const app = await openApp(browser, { session: sessionA })
    const target = '/tokens?probe=cross-tab#list'
    await visit(app, target)
    await app.page.locator('.token-table').waitFor()
    const logout = kind !== 'request'
    const reply = kind === 'logout' ? json({ status: 'ok' }) : kind === 'logout-network' ? { abort: true } : failure()
    const late = hold(app, logout ? 'POST /api/admin/logout' : 'GET /api/admin/keys', reply)
    if (logout) await app.page.locator('.float-logout').click()
    else await startRequests(app, ['/api/admin/keys?probe=cross-tab-late'])
    assert.equal((await late.seen).cookie, sessionA)
    app.sessions.delete(sessionA)
    const tab = await newTab(app, '/login?redirect=%2Fproviders')
    await onLogin(tab, '/providers')
    await login(tab, '/providers')
    await app.page.evaluate(/** Records transient login rendering that a final DOM snapshot would miss. */ () => {
      window.authProbe.loginShown = false
      window.authProbe.loginObserver = new MutationObserver(/** Observes the whole late-response window for an unwanted login form. */ () => {
        if (document.querySelector('.login-card')) window.authProbe.loginShown = true
      })
      window.authProbe.loginObserver.observe(document.body, { childList: true, subtree: true })
    })
    late.release()
    if (!logout) {
      await rejected(app, ['admin login required'])
      await at(app, target)
    } else if (kind === 'logout-network') {
      await app.page.locator('.logout-error').getByText('Failed to fetch', { exact: true }).waitFor()
      await at(app, target)
    } else {
      await at(app, '/playground')
      await app.page.locator('.search-input input').waitFor()
    }
    assert.equal(await app.page.evaluate(/** Releases the observer before reporting whether login was ever rendered. */ () => {
      window.authProbe.loginObserver.disconnect()
      return window.authProbe.loginShown
    }), false)
    assert.equal(await app.page.locator('.login-card').count(), 0)
    await sessionIs(app, sessionB)
    await sessionIs(tab, sessionB)
    assert.equal(app.sessions.has(sessionB), true)
    await closeApp(app)
  }
}

/** Builds metadata-only entries with explicit nullable identity, status and execution fields. */
function userLog(overrides = {}) {
  return {
    id: 41, request_id: 'entry-41', created_at: '2026-09-11T01:02:03Z',
    operation: 'search', compat_format: 'native', method: 'POST', path: '/v1/search',
    client_ip: '2001:db8::41', auth_type: 'api_token', api_token_id: 17,
    token_name: 'fixture-retired-token', http_status: 200, completion: 'completed',
    latency_ms: 0, mcp_error_count: 0, mcp_tool_error_count: 0,
    execution_request_id: null, ...overrides
  }
}

/** Keeps historical execution fixtures free of invented HTTP or caller metadata. */
function executionLog(overrides = {}) {
  return {
    id: 41, request_id: 'historical-41', created_at: '2026-09-10T01:02:03Z',
    operation: 'search', query: 'historical response-only search', mode: 'fallback',
    compat_format: 'native', providers: ['tavily'], cache_policy: 'auto',
    cache_hit: false, result_count: 1, status: 'success', error_message: '',
    latency_ms: 34, request_json: {}, response_json: {}, ...overrides
  }
}

/** Provides retries, cached results and both legacy call fallbacks without upstream traffic. */
function logFixtures() {
  const result = { title: 'fixture result title', url: 'https://fixture.invalid/result', snippet: 'fixture result snippet', provider: 'tavily' }
  const call = {
    provider_key_id: 31, provider_name: 'tavily', key_alias: 'fixture-success-key',
    attempt_index: 2, will_retry: false, status: 'success', error_type: '',
    error_message: '', latency_ms: 14, result_count: 1, cached: false
  }
  const group = { provider: 'tavily', status: 'success', latency_ms: 14, result_count: 1, results: [result] }
  const entries = [
    userLog({ execution_request_id: 'entry-41' }),
    userLog({ id: 42, request_id: 'entry-42', auth_type: 'unknown', api_token_id: null, token_name: '', client_ip: '', http_status: 401 }),
    userLog({ id: 43, request_id: 'entry-43', token_name: 'fixture-rate-limited', api_token_id: 18, client_ip: '198.51.100.43', http_status: 429 }),
    userLog({ id: 44, request_id: 'entry-44', operation: 'mcp', path: '/v1/mcp', auth_type: 'anonymous', api_token_id: null, token_name: '', mcp_error_count: 2 }),
    userLog({ id: 45, request_id: 'entry-45', operation: 'mcp', path: '/custom-mcp', mcp_error_count: 1, mcp_tool_error_count: 1, execution_request_id: 'entry-45-mcp-7-2' }),
    userLog({ id: 46, request_id: 'entry-46', operation: 'mcp', path: '/v1/mcp', auth_type: 'anonymous', api_token_id: null, token_name: '', http_status: 202 }),
    userLog({ id: 47, request_id: 'entry-47', operation: 'mcp', method: 'GET', path: '/custom-mcp', auth_type: 'unknown', api_token_id: null, token_name: '', http_status: null, completion: 'interrupted' }),
    userLog({ id: 48, request_id: 'entry-48', operation: 'extract', compat_format: 'tavily', path: '/v1/compat/tavily/extract', http_status: 403, execution_request_id: 'entry-48' }),
    userLog({ id: 49, request_id: 'entry-49', execution_request_id: 'entry-49' }),
    userLog({ id: 50, request_id: 'entry-50', operation: 'mcp', method: 'DELETE', path: '/custom-mcp', auth_type: 'admin_key', api_token_id: null, token_name: '', http_status: 204 }),
    userLog({ id: 51, request_id: 'entry-51', http_status: 302 }),
    userLog({ id: 52, request_id: 'entry-52', http_status: 502, completion: 'write_error' }),
    userLog({ id: 53, request_id: 'entry-53', http_status: null, completion: 'canceled' })
  ]
  const executions = [
    { log: executionLog({ response_json: { provider_calls: [call], provider_results: [group] } }), calls: [] },
    { log: executionLog({ id: 42, request_id: 'historical-42', query: 'historical group-only search', response_json: { provider_results: [group] } }), calls: [] },
    { log: executionLog({ id: 43, request_id: 'historical-43', query: 'historical summary-only search' }), calls: [call] },
    {
      log: executionLog({
        id: 44, request_id: 'historical-44', operation: 'extract', query: 'historical extraction',
        request_json: { urls: ['https://fixture.invalid/extract'], providers: ['tavily'] },
        response_json: { results: [{ ...result, title: 'fixture extract title', content: 'fixture extract content', content_truncated: true, log_content_truncated: true }] }
      }),
      calls: []
    },
    {
      log: executionLog({ id: 901, request_id: 'entry-41', query: 'linked retry search', response_json: { results: [result], provider_results: [group] } }),
      calls: [
        { ...call, provider_key_id: 30, key_alias: 'fixture-retry-key', attempt_index: 1, will_retry: true, status: 'error', error_type: 'rate_limited', error_message: 'fixture rate limit', result_count: 0 },
        call
      ]
    },
    { log: executionLog({ id: 902, request_id: 'entry-49', query: 'linked cached search', cache_hit: true, response_json: { results: [result] } }), calls: [] },
    { log: executionLog({ id: 903, request_id: 'entry-45-mcp-7-2', query: 'linked MCP failure', status: 'error', error_message: 'fixture tool failure', result_count: 0 }), calls: [] },
    { log: executionLog({ id: 904, request_id: 'entry-48', operation: 'extract', query: '', status: 'error', error_message: 'fixture extract scope rejected', result_count: 0, providers: [] }), calls: [] }
  ]
  return { entries, executions, recent: executions.slice(0, 4).map((detail) => detail.log) }
}

/** Installs explicit list and numeric-detail responses; every unlisted route still fails. */
function mockLogs(app, fixture) {
  app.overrides.set('GET /api/admin/request-logs', json({ logs: fixture.entries }))
  app.overrides.set('GET /api/admin/logs', json({ logs: fixture.recent }))
  for (const log of fixture.entries) {
    const execution = fixture.executions.find((detail) => detail.log.request_id === log.execution_request_id)
    app.overrides.set('GET /api/admin/request-logs/' + log.id, json({ log, execution_log_id: execution?.log.id ?? null }))
  }
  for (const detail of fixture.executions) app.overrides.set('GET /api/admin/logs/' + detail.log.id, json(detail))
}

/** Observes original API promises so stale-success/catch/finally assertions wait for actual settlement. */
async function observeLogReads(app) {
  await app.page.evaluate(/** Returns each original promise to the view, storing only a separate settlement observer. */ async () => {
    const { api } = await import('/src/api/client.ts')
    window.authProbe.logReads = []
    for (const [name, path, detail] of [
      ['userRequestLogs', '/api/admin/request-logs', false],
      ['userRequestLogDetail', '/api/admin/request-logs', true],
      ['logs', '/api/admin/logs', false],
      ['logDetail', '/api/admin/logs', true]
    ]) {
      const read = api[name]
      if (typeof read !== 'function') throw new Error('Missing log API method: ' + name)
      /** Preserves the real fetch, body parsing and session recovery without replay or synthetic results. */
      api[name] = (...args) => {
        const pending = read(...args)
        const record = { path: path + (detail ? '/' + args[0] : '') }
        record.pending = pending.then(
          /** Records completion without retaining execution bodies or changing the view's promise. */
          () => ({ status: 'fulfilled' }),
          /** Observes rejection without suppressing it for the view that owns error presentation. */
          (error) => ({ status: 'rejected', message: error.message })
        )
        window.authProbe.logReads.push(record)
        return pending
      }
    }
  })
}

/** Waits for a read and Vue updates; paused timer cases advance transition frames without resuming real time. */
async function logReadSettled(app, path, index = -1) {
  const outcome = await bounded(app.page.evaluate(/** Awaits the registered API observer after the view has subscribed to the same original promise. */ async ({ path, index }) => {
    const record = window.authProbe.logReads.filter((read) => read.path === path).at(index)
    if (!record) throw new Error('No observed log request: ' + path)
    const outcome = await record.pending
    await Promise.resolve()
    await Promise.resolve()
    return outcome
  }, { path, index }), 'log read settlement: ' + path)
  // Vue transitions schedule two animation frames before leaving loading overlays.
  if (app.logClockPaused) await app.page.clock.runFor(50)
  return outcome
}

/** Observes reads before the first log view; timer cases install before app load and pause only after readiness. */
async function openLogs(browser, { viewport = desktop, fixture = logFixtures(), clock = false, initialReply } = {}) {
  const app = await openApp(browser, { session: sessionA, viewport })
  if (clock) await app.page.clock.install({ time: new Date('2026-09-11T00:00:00Z') })
  mockLogs(app, fixture)
  await visit(app, '/tokens')
  await app.page.locator('.token-table').waitFor()
  await observeLogReads(app)
  if (initialReply) app.overrides.set('GET /api/admin/request-logs', initialReply)
  await navigate(app, '/logs')
  await logReadSettled(app, '/api/admin/request-logs')
  if (clock) {
    // Polling is already disabled by at(); allow setup timers to settle before freezing time.
    const pauseTime = await app.page.evaluate(() => Date.now() + 60000)
    await app.page.clock.pauseAt(pauseTime)
    app.logClockPaused = true
  }
  return app
}

/** Selects page tabs separately from the execution drawer's parameter/result tabs. */
async function logView(app, kind) {
  const tab = app.page.locator('.logs-view-tabs').getByRole('tab', { name: kind === 'user' ? '\u7528\u6237\u8bf7\u6c42' : '\u6267\u884c\u65e5\u5fd7', exact: true })
  await tab.click()
  assert.equal(await tab.getAttribute('aria-selected'), 'true')
}

/** Targets row identity within the page so colliding entry/execution IDs cannot select drawer metadata. */
function logRow(app, kind, id) {
  return app.page.locator('.logs-page [data-log-kind="' + kind + '"][data-log-id="' + id + '"]')
}

/** Requires the exact visible row set; null statuses and hidden tabs cannot silently join a filter. */
async function logRowsAre(app, kind, ids) {
  await app.page.waitForFunction(/** Compares visible row identities rather than text that can appear in a hidden drawer. */ ({ kind, ids }) => {
    const rows = [...document.querySelectorAll('.logs-page [data-log-kind][data-log-id]')].filter((row) => row.getClientRects().length)
    return rows.every((row) => row.dataset.logKind === kind) &&
      JSON.stringify(rows.map((row) => Number(row.dataset.logId)).sort((a, b) => a - b)) === JSON.stringify(ids)
  }, { kind, ids: [...ids].sort((a, b) => a - b) }, { polling: 50 })
}

/** Selects or clears one visible page filter using the rendered Element Plus controls. */
async function logFilter(app, index, label) {
  const select = app.page.locator('.logs-page .el-select:visible').nth(index)
  if (label === null) {
    await select.hover()
    await select.locator('.el-select__clear').click()
  } else {
    await select.click()
    await app.page.locator('.el-select-dropdown:visible').getByRole('option', { name: label, exact: true }).click()
  }
}

/** Opens a numeric detail from its visible row and checks the shared drawer has only one active instance. */
async function openLogDetail(app, kind, id) {
  await logRow(app, kind, id).click()
  const outcome = await logReadSettled(app, (kind === 'user' ? '/api/admin/request-logs/' : '/api/admin/logs/') + id)
  const drawer = app.page.locator('.log-drawer.open')
  await drawer.waitFor()
  assert.equal(await app.page.locator('.log-drawer').count(), 1)
  return { drawer, outcome }
}

/** Closes through the keyboard and waits until the old selection can no longer render a drawer. */
async function closeLogDetail(app) {
  const drawer = app.page.locator('.log-drawer.open')
  await drawer.press('Escape')
  await drawer.waitFor({ state: 'hidden' })
}

/** Checks page, row and detail scroll widths so nested clipping cannot conceal long-metadata overflow. */
async function logBounds(app) {
  const viewport = app.page.viewportSize()
  const layout = await app.page.evaluate(/** Measures rendered boundaries without hiding overflow or relying on PNG width. */ () => {
    const selectors = ['html', 'body', '#app', '.logs-page', '.logs-view-tabs', '.logs-actions']
    const elements = selectors.map((selector) => {
      const element = document.querySelector(selector)
      if (!element) throw new Error('Missing log layout boundary: ' + selector)
      return { selector, element }
    })
    for (const selector of [
      '.logs-page .stream', '.logs-page .user-log-card', '.user-log-card .body',
      '.user-log-card .endpoint', '.user-log-card .request-id', '.user-log-card .entry-caller',
      '.log-drawer.open', '.log-drawer.open .dhd', '.log-drawer.open .entry-detail',
      '.log-drawer.open .entry-metadata dd'
    ]) {
      for (const element of document.querySelectorAll(selector)) {
        if (element.getClientRects().length) elements.push({ selector, element })
      }
    }
    return elements.map(({ selector, element }) => {
      const rect = element.getBoundingClientRect()
      return { selector, left: rect.left, right: rect.right, top: rect.top, bottom: rect.bottom, client: element.clientWidth, scroll: element.scrollWidth }
    })
  })
  for (const element of layout) {
    assert.ok(element.left >= -1 && element.right <= viewport.width + 1, element.selector + ' escaped viewport: ' + JSON.stringify(element))
    assert.ok(element.scroll <= element.client + 1, element.selector + ' overflowed: ' + JSON.stringify(element))
    if (element.selector === '.log-drawer.open') {
      assert.ok(element.top >= -1 && element.bottom <= viewport.height + 1, 'Drawer escaped viewport height')
    }
  }
}

/** Exercises independent recent-window filters, transport/MCP distinctions and keyboard/layout behavior. */
async function populatedLogs(browser, viewport, name) {
  const fixture = logFixtures()
  const long = userLog({
    id: 54, request_id: 'long-request-' + 'r'.repeat(220), operation: 'mcp',
    path: '/custom-mcp/' + 'endpoint'.repeat(240), token_name: 'fixture-name-' + 'n'.repeat(240),
    client_ip: '2001:db8:abcd:1234:5678:90ab:cdef:1234'
  })
  long.execution_request_id = long.request_id + '-mcp-7-1'
  fixture.entries.push(long)
  fixture.executions.push({ log: executionLog({ id: 905, request_id: long.execution_request_id, query: 'long entry execution' }), calls: [] })
  const app = await openLogs(browser, { viewport, fixture })
  const all = fixture.entries.map((row) => row.id)
  const search = app.page.locator('[data-log-filter="user-text"] input')
  await logRowsAre(app, 'user', all)
  assert.equal(await app.page.locator('.logs-view-tabs [role="tab"][aria-selected="true"]').innerText(), '\u7528\u6237\u8bf7\u6c42')
  assert.equal(await app.page.locator('.kpi-row').count(), 0, 'Execution totals appeared in the entry view')
  assert.equal(app.requests.some((request) => request.key === 'GET /api/admin/logs'), false)
  const first = logRow(app, 'user', 41)
  for (const value of ['POST', '/v1/search', 'entry-41', '2001:db8::41', 'fixture-retired-token', '0ms']) {
    assert.ok((await first.innerText()).includes(value), 'Missing entry field: ' + value)
  }
  assert.match(await first.locator('.entry-caller').innerText(), /17/)
  assert.match(await first.locator('.meta').innerText(), /2026/)
  assert.match(await logRow(app, 'user', 44).innerText(), /HTTP 200/)
  await logRow(app, 'user', 44).getByText('\u534f\u8bae\u9519\u8bef 2', { exact: true }).waitFor()
  await logRow(app, 'user', 45).getByText('\u5de5\u5177\u9519\u8bef 1', { exact: true }).waitFor()
  assert.match(await logRow(app, 'user', 46).innerText(), /HTTP 202/)
  for (const id of [47, 53]) {
    const outcomes = await logRow(app, 'user', id).locator('.entry-outcomes').innerText()
    assert.match(outcomes, /\u672a\u8bb0\u5f55/)
    assert.doesNotMatch(outcomes, /\b(?:200|500)\b/)
  }
  assert.doesNotMatch(await logRow(app, 'user', 42).locator('.entry-caller').innerText(), /fixture-|#0|\u533f\u540d/)
  assert.match(await logRow(app, 'user', 50).innerText(), /DELETE/)
  assert.match(await logRow(app, 'user', 50).locator('.entry-caller').innerText(), /\u7ba1\u7406\u5458 Key/)
  assert.match(await logRow(app, 'user', 46).locator('.entry-caller').innerText(), /\u533f\u540d/)
  for (const [id, text] of [[41, '\u5df2\u5b8c\u6210'], [47, '\u5df2\u4e2d\u65ad'], [52, '\u54cd\u5e94\u5199\u5165\u5931\u8d25'], [53, '\u5df2\u53d6\u6d88']]) {
    await logRow(app, 'user', id).getByText(text, { exact: true }).waitFor()
  }
  for (const id of [44, 45, 47, 52, 53]) assert.match(await logRow(app, 'user', id).getAttribute('class'), /\bfail\b/)
  assert.doesNotMatch(await logRow(app, 'user', 46).getAttribute('class'), /\bfail\b/)
  await logBounds(app)
  await screenshot(app, name + '-user-logs')

  for (const [text, ids] of [
    ['entry-41', [41]], ['/v1/compat/tavily/extract', [48]],
    ['198.51.100.43', [43]], ['FIXTURE-RATE-LIMITED', [43]], ['no-matching-request', []]
  ]) {
    await search.fill(text)
    await logRowsAre(app, 'user', ids)
  }
  await app.page.locator('.user-stream .empty').waitFor()
  await search.fill('')
  for (const [label, ids] of [
    ['2xx', [41, 44, 45, 46, 49, 50, 54]], ['3xx', [51]],
    ['4xx', [42, 43, 48]], ['5xx', [52]], ['\u672a\u8bb0\u5f55', [47, 53]]
  ]) {
    await logFilter(app, 1, label)
    await logRowsAre(app, 'user', ids)
  }
  await logFilter(app, 1, null)
  await logFilter(app, 0, 'MCP')
  await logRowsAre(app, 'user', [44, 45, 46, 47, 50, 54])
  await logFilter(app, 1, '2xx')
  for (const [label, ids] of [
    ['\u534f\u8bae\u6216\u5de5\u5177\u9519\u8bef', [44, 45]],
    ['\u534f\u8bae\u9519\u8bef', [44, 45]], ['\u5de5\u5177\u9519\u8bef', [45]]
  ]) {
    await logFilter(app, 2, label)
    await logRowsAre(app, 'user', ids)
  }
  await logFilter(app, 2, null)
  await logFilter(app, 1, null)
  await logFilter(app, 0, '\u62bd\u53d6')
  await logRowsAre(app, 'user', [48])
  await logFilter(app, 0, null)
  await search.fill('entry-41')

  await logView(app, 'execution')
  await logReadSettled(app, '/api/admin/logs')
  await logRowsAre(app, 'execution', [41, 42, 43, 44])
  await app.page.locator('.kpi-row').waitFor()
  const executionSearch = app.page.locator('[data-log-filter="execution-text"] input')
  await executionSearch.fill('historical-44')
  await logRowsAre(app, 'execution', [44])
  await logView(app, 'user')
  await logReadSettled(app, '/api/admin/request-logs')
  await logRowsAre(app, 'user', [41])
  assert.equal(await search.inputValue(), 'entry-41')
  assert.match(await app.page.locator('.window-count').innerText(), /1\s*\/.*14/)
  await logView(app, 'execution')
  await logReadSettled(app, '/api/admin/logs')
  await logRowsAre(app, 'execution', [44])
  const { drawer: executionDrawer } = await openLogDetail(app, 'execution', 44)
  await executionDrawer.getByRole('tab', { name: /^\u62bd\u53d6\u7ed3\u679c/ }).click()
  await executionDrawer.locator('.result-expand-button:visible').click()
  await executionDrawer.getByText('fixture extract content', { exact: true }).waitFor()
  await logBounds(app)
  await screenshot(app, name + '-execution-log')
  await closeLogDetail(app)

  await logView(app, 'user')
  await logReadSettled(app, '/api/admin/request-logs')
  await search.fill('long-request-')
  await logRowsAre(app, 'user', [54])
  const row = logRow(app, 'user', 54)
  await row.focus()
  await row.press('Enter')
  await logReadSettled(app, '/api/admin/request-logs/54')
  const drawer = app.page.locator('.log-drawer.open')
  await drawer.getByText(long.path, { exact: true }).waitFor()
  assert.ok((await drawer.innerText()).includes(long.token_name))
  assert.ok((await drawer.innerText()).includes(long.request_id))
  await app.page.waitForFunction(() => document.querySelector('.log-drawer.open')?.contains(document.activeElement))
  await logBounds(app)
  await screenshot(app, name + '-user-log-detail')
  const link = drawer.getByRole('button', { name: '\u67e5\u770b\u6267\u884c\u65e5\u5fd7', exact: true })
  await link.scrollIntoViewIfNeeded()
  const scroll = await drawer.locator('.entry-detail').evaluate((element) => element.scrollTop)
  assert.ok(scroll > 0, 'Long entry did not exercise scrolling detail content')
  const listsBefore = app.requests.filter((request) => request.key === 'GET /api/admin/request-logs').length
  await link.click()
  await logReadSettled(app, '/api/admin/logs/905')
  const back = drawer.getByRole('button', { name: '\u8fd4\u56de\u7528\u6237\u8bf7\u6c42', exact: true })
  await back.focus()
  await back.press('Enter')
  await drawer.locator('[data-execution-state="linked"]').waitFor()
  await app.page.waitForFunction((scroll) => Math.abs(document.querySelector('.log-drawer.open .entry-detail').scrollTop - scroll) <= 1, scroll)
  assert.equal(await link.evaluate((element) => element === document.activeElement), true, 'Back did not return focus to the execution link')
  assert.equal(app.requests.filter((request) => request.key === 'GET /api/admin/request-logs').length, listsBefore)
  await drawer.getByRole('button', { name: '\u5173\u95ed\u8be6\u60c5', exact: true }).focus()
  await app.page.keyboard.press('Shift+Tab')
  assert.equal(await link.evaluate((element) => element === document.activeElement), true, 'Drawer keyboard focus escaped its first control')
  await app.page.keyboard.press('Tab')
  assert.equal(await drawer.getByRole('button', { name: '\u5173\u95ed\u8be6\u60c5', exact: true }).evaluate((element) => element === document.activeElement), true)
  await closeLogDetail(app)
  assert.equal(await row.evaluate((element) => element === document.activeElement), true, 'Closing detail did not restore row focus')
  await logBounds(app)
  await sessionIs(app, sessionA)
  await closeApp(app)
}

/** Follows exact numeric links outside the execution window and preserves retry/cache/legacy result details. */
async function logCorrelation(browser) {
  const app = await openLogs(browser)
  const search = app.page.locator('[data-log-filter="user-text"] input')
  for (const [entryId, executionId] of [[41, 901], [49, 902], [45, 903], [48, 904]]) {
    await search.fill('entry-' + entryId)
    await logRowsAre(app, 'user', [entryId])
    const { drawer } = await openLogDetail(app, 'user', entryId)
    await drawer.locator('[data-execution-state="linked"]').waitFor()
    await drawer.getByRole('button', { name: '\u67e5\u770b\u6267\u884c\u65e5\u5fd7', exact: true }).click()
    assert.deepEqual(await logReadSettled(app, '/api/admin/logs/' + executionId), { status: 'fulfilled' })
    assert.equal(await drawer.getAttribute('data-detail-kind'), 'execution')
    assert.equal(await drawer.getAttribute('data-detail-id'), String(executionId))
    assert.equal(await app.page.locator('.log-drawer').count(), 1)
    assert.equal(app.requests.some((request) => request.key === 'GET /api/admin/logs'), false, 'Link resolution fetched/searched the recent execution window')
    if (entryId === 41) {
      assert.equal(await drawer.locator('.call-card').count(), 2)
      const failed = drawer.locator('.call-card').nth(0)
      const succeeded = drawer.locator('.call-card').nth(1)
      assert.match(await failed.innerText(), /fixture-retry-key/)
      assert.match(await failed.innerText(), /\u5c06\u91cd\u8bd5/)
      await failed.locator('.call-top').click()
      assert.equal(await failed.locator('.result-title-link').count(), 0, 'Retry failure borrowed successful attempt results')
      await succeeded.locator('.call-top').click()
      await succeeded.locator('.result-title-link').waitFor()
      await succeeded.locator('.result-expand-button').click()
      await succeeded.getByText('fixture result snippet', { exact: true }).waitFor()
      await screenshot(app, 'desktop-linked-retry-execution')
    } else if (entryId === 49) {
      assert.equal(await drawer.locator('.call-card').count(), 0, 'Cache hit fabricated provider attempts')
      await drawer.getByRole('tab', { name: /^\u641c\u7d22\u7ed3\u679c/ }).click()
      await drawer.locator('.result-title-link:visible').waitFor()
    } else {
      await drawer.getByRole('tab', { name: '\u8bf7\u6c42\u53c2\u6570', exact: true }).click()
      assert.match(await drawer.innerText(), entryId === 45 ? /entry-45-mcp-7-2/ : /fixture extract scope rejected/)
    }
    await drawer.getByRole('button', { name: '\u8fd4\u56de\u7528\u6237\u8bf7\u6c42', exact: true }).click()
    await drawer.locator('[data-execution-state="linked"]').waitFor()
    assert.equal(await drawer.getAttribute('data-detail-kind'), 'user')
    assert.equal(await drawer.getAttribute('data-detail-id'), String(entryId))
    assert.equal(await drawer.locator('.call-card').count(), 0)
    await closeLogDetail(app)
    await logRowsAre(app, 'user', [entryId])
    assert.equal(await search.inputValue(), 'entry-' + entryId)
  }
  await logView(app, 'execution')
  await logReadSettled(app, '/api/admin/logs')
  for (const id of [41, 42, 43]) {
    const { drawer } = await openLogDetail(app, 'execution', id)
    assert.match(await drawer.locator('.dhd').innerText(), new RegExp('historical-' + id))
    assert.equal(await drawer.locator('.entry-metadata').count(), 0, 'Historical execution acquired fabricated entry metadata')
    assert.equal(await drawer.getByRole('button', { name: '\u8fd4\u56de\u7528\u6237\u8bf7\u6c42', exact: true }).count(), 0)
    assert.equal(await drawer.locator('.call-card').count(), 1)
    await drawer.locator('.call-top').click()
    if (id === 43) {
      await drawer.getByText(/\u6b63\u6587\u672a\u5199\u5165\u65e5\u5fd7/).waitFor()
      assert.equal(await drawer.locator('.result-title-link').count(), 0)
    } else {
      await drawer.locator('.result-title-link').waitFor()
      await drawer.locator('.result-expand-button').click()
      await drawer.getByText('fixture result snippet', { exact: true }).waitFor()
    }
    await closeLogDetail(app)
  }
  await sessionIs(app, sessionA)
  await closeApp(app)
}

/** Distinguishes initial/refresh errors, no execution, missing intended data and retryable detail failures. */
async function logReadErrors(browser) {
  const fixture = logFixtures()
  const app = await openLogs(browser, { fixture, initialReply: failure(503, 'fixture entry list unavailable') })
  const listError = app.page.locator('.list-error')
  await listError.getByText('fixture entry list unavailable', { exact: true }).waitFor()
  assert.equal(await app.page.locator('.user-stream .empty:visible').count(), 0, 'Initial failure was rendered as an empty success')
  await logView(app, 'execution')
  await logReadSettled(app, '/api/admin/logs')
  await logRowsAre(app, 'execution', [41, 42, 43, 44])
  await logView(app, 'user')
  await logReadSettled(app, '/api/admin/request-logs')
  await listError.waitFor()
  app.overrides.set('GET /api/admin/request-logs', json({ logs: fixture.entries }))
  await listError.getByRole('button', { name: '\u91cd\u8bd5', exact: true }).click()
  await logReadSettled(app, '/api/admin/request-logs')
  await logRowsAre(app, 'user', fixture.entries.map((row) => row.id))

  for (const [kind, path] of [['user', '/api/admin/request-logs'], ['execution', '/api/admin/logs']]) {
    await logView(app, kind)
    await logReadSettled(app, path)
    app.overrides.set('GET ' + path, failure(500, 'fixture refresh unavailable'))
    await app.page.locator('.logs-actions button[title="\u5237\u65b0"]').click()
    await logReadSettled(app, path)
    await listError.getByText('fixture refresh unavailable', { exact: true }).waitFor()
    await logRowsAre(app, kind, (kind === 'user' ? fixture.entries : fixture.recent).map((row) => row.id))
    app.overrides.set('GET ' + path, json({ logs: kind === 'user' ? fixture.entries : fixture.recent }))
    await listError.getByRole('button', { name: '\u91cd\u8bd5', exact: true }).click()
    await logReadSettled(app, path)
    await listError.waitFor({ state: 'hidden' })
  }
  await logView(app, 'user')
  await logReadSettled(app, '/api/admin/request-logs')
  for (const id of [42, 46]) {
    const { drawer } = await openLogDetail(app, 'user', id)
    await drawer.locator('[data-execution-state="none"]').getByText('\u672a\u8fdb\u5165\u6267\u884c\u6d41\u7a0b', { exact: true }).waitFor()
    assert.equal(await drawer.getByRole('button', { name: '\u67e5\u770b\u6267\u884c\u65e5\u5fd7', exact: true }).count(), 0)
    await closeLogDetail(app)
  }
  const intended = fixture.entries.find((row) => row.id === 48)
  app.overrides.set('GET /api/admin/request-logs/48', json({ log: intended, execution_log_id: null }))
  const { drawer } = await openLogDetail(app, 'user', 48)
  await drawer.locator('[data-execution-state="missing"]').getByText('\u6267\u884c\u8bb0\u5f55\u6682\u4e0d\u53ef\u7528', { exact: true }).waitFor()
  assert.equal(await drawer.getByText('\u672a\u8fdb\u5165\u6267\u884c\u6d41\u7a0b', { exact: true }).count(), 0)
  assert.equal(await drawer.getByRole('button', { name: '\u67e5\u770b\u6267\u884c\u65e5\u5fd7', exact: true }).count(), 0)
  await closeLogDetail(app)

  const metadata = fixture.entries.find((row) => row.id === 41)
  for (const status of [404, 500]) {
    app.overrides.set('GET /api/admin/request-logs/41', failure(status, 'fixture entry detail unavailable'))
    await openLogDetail(app, 'user', 41)
    const current = app.page.locator('.log-drawer.open')
    await current.locator('.detail-error').getByText(status === 404 ? /\u7528\u6237\u8bf7\u6c42\u8bb0\u5f55\u4e0d\u5b58\u5728\u6216\u5df2\u6e05\u7406/ : /fixture entry detail unavailable/).waitFor()
    assert.equal(await current.locator('[data-execution-state="none"], [data-execution-state="missing"]').count(), 0)
    app.overrides.set('GET /api/admin/request-logs/41', json({ log: metadata, execution_log_id: 901 }))
    await current.getByRole('button', { name: '\u91cd\u8bd5', exact: true }).click()
    await logReadSettled(app, '/api/admin/request-logs/41')
    await current.locator('[data-execution-state="linked"]').waitFor()
    await closeLogDetail(app)
  }
  await openLogDetail(app, 'user', 41)
  const current = app.page.locator('.log-drawer.open')
  app.overrides.set('GET /api/admin/logs/901', failure(500, 'fixture linked execution unavailable'))
  await current.getByRole('button', { name: '\u67e5\u770b\u6267\u884c\u65e5\u5fd7', exact: true }).click()
  await logReadSettled(app, '/api/admin/logs/901')
  await current.locator('.detail-error').getByText(/fixture linked execution unavailable/).waitFor()
  assert.equal(await current.locator('.drawer-tabs').count(), 0, 'Failed detail became an empty execution success')
  await current.getByRole('button', { name: '\u8fd4\u56de\u7528\u6237\u8bf7\u6c42', exact: true }).click()
  await current.locator('[data-execution-state="linked"]').waitFor()
  await current.getByRole('button', { name: '\u67e5\u770b\u6267\u884c\u65e5\u5fd7', exact: true }).click()
  await logReadSettled(app, '/api/admin/logs/901')
  app.overrides.set('GET /api/admin/logs/901', json(fixture.executions.find((detail) => detail.log.id === 901)))
  await current.getByRole('button', { name: '\u91cd\u8bd5', exact: true }).click()
  await logReadSettled(app, '/api/admin/logs/901')
  await current.locator('.call-card').first().waitFor()
  assert.equal(await current.locator('.call-card').count(), 2)
  await current.getByRole('button', { name: '\u8fd4\u56de\u7528\u6237\u8bf7\u6c42', exact: true }).click()
  await closeLogDetail(app)
  await sessionIs(app, sessionA)
  assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 0)
  await screenshot(app, 'desktop-log-error-recovered')
  await closeApp(app)
}

/** Rejects old list success/error/finally work across tab-away/back even when the final kind matches. */
async function logListRaces(browser) {
  for (const kind of ['user', 'execution']) {
    for (const staleFails of [false, true]) {
      const fixture = logFixtures()
      const app = await openLogs(browser, { fixture })
      const path = kind === 'user' ? '/api/admin/request-logs' : '/api/admin/logs'
      const other = kind === 'user' ? 'execution' : 'user'
      await logView(app, kind)
      await logReadSettled(app, path)
      const before = kind === 'user' ? fixture.entries : fixture.recent
      const stale = kind === 'user' ? userLog({ id: 201, request_id: 'stale-list-entry' }) : executionLog({ id: 201, query: 'stale-list-execution' })
      const newest = kind === 'user' ? userLog({ id: 202, request_id: 'current-list-entry' }) : executionLog({ id: 202, query: 'current-list-execution' })
      const old = hold(app, 'GET ' + path, staleFails ? failure(500, 'fixture stale list error') : json({ logs: [stale] }))
      await app.page.locator('.logs-actions button[title="\u5237\u65b0"]').click()
      await bounded(old.seen, 'old list arrival')
      const oldIndex = await app.page.evaluate((path) => window.authProbe.logReads.filter((read) => read.path === path).length - 1, path)
      await logView(app, other)
      await logReadSettled(app, other === 'user' ? '/api/admin/request-logs' : '/api/admin/logs')
      const next = hold(app, 'GET ' + path, json({ logs: [newest] }))
      await logView(app, kind)
      await bounded(next.seen, 'new list arrival')
      // A stale finally must not clear the new request's pending indicator.
      old.release()
      await logReadSettled(app, path, oldIndex)
      await app.page.locator('.logs-actions button.is-loading').waitFor()
      await logRowsAre(app, kind, before.map((row) => row.id))
      assert.equal(await app.page.locator('.list-error').count(), 0)
      next.release()
      await logReadSettled(app, path)
      await logRowsAre(app, kind, [202])
      assert.equal(await app.page.locator('.list-error').count(), 0)

      const late = hold(app, 'GET ' + path, staleFails ? failure(500, 'fixture late list error') : json({ logs: [stale] }))
      await app.page.locator('.logs-actions button[title="\u5237\u65b0"]').click()
      await bounded(late.seen, 'late list arrival')
      const lateIndex = await app.page.evaluate((path) => window.authProbe.logReads.filter((read) => read.path === path).length - 1, path)
      app.overrides.set('GET ' + path, json({ logs: [newest] }))
      await logView(app, other)
      await logReadSettled(app, other === 'user' ? '/api/admin/request-logs' : '/api/admin/logs')
      await logView(app, kind)
      await logReadSettled(app, path)
      await logRowsAre(app, kind, [202])
      late.release()
      await logReadSettled(app, path, lateIndex)
      await logRowsAre(app, kind, [202])
      assert.equal(await app.page.locator('.list-error').count(), 0)
      const unmounted = hold(app, 'GET ' + path, staleFails ? failure(500, 'unmounted-list-sentinel') : json({ logs: [stale] }))
      await app.page.locator('.logs-actions button[title="\u5237\u65b0"]').click()
      await bounded(unmounted.seen, 'unmounted list arrival')
      await navigate(app, '/tokens')
      unmounted.release()
      await logReadSettled(app, path)
      assert.equal(await app.page.locator('.logs-page, .list-error, .log-drawer.open').count(), 0)
      await closeApp(app)
    }
  }
}

/** Rejects old entry/execution detail success and failure after new selection, back, close or unmount. */
async function logDetailRaces(browser) {
  for (const staleFails of [false, true]) {
    const fixture = logFixtures()
    const app = await openLogs(browser, { fixture })
    for (const kind of ['user', 'execution']) {
      await logView(app, kind)
      await logReadSettled(app, kind === 'user' ? '/api/admin/request-logs' : '/api/admin/logs')
      const path = kind === 'user' ? '/api/admin/request-logs/' : '/api/admin/logs/'
      const oldBody = kind === 'user'
        ? { log: userLog({ token_name: 'stale-detail-sentinel' }), execution_log_id: 999 }
        : { log: executionLog({ query: 'stale-detail-sentinel' }), calls: [] }
      const old = hold(app, 'GET ' + path + '41', staleFails ? failure(500, 'stale-detail-sentinel') : json(oldBody))
      await logRow(app, kind, 41).click()
      await bounded(old.seen, 'old detail arrival')
      const oldIndex = await app.page.evaluate((path) => window.authProbe.logReads.filter((read) => read.path === path).length - 1, path + '41')
      await closeLogDetail(app)
      const nextBody = kind === 'user'
        ? { log: fixture.entries.find((row) => row.id === 42), execution_log_id: null }
        : fixture.executions.find((detail) => detail.log.id === 42)
      const next = hold(app, 'GET ' + path + '42', json(nextBody))
      await logRow(app, kind, 42).click()
      await bounded(next.seen, 'new detail arrival')
      old.release()
      await logReadSettled(app, path + '41', oldIndex)
      const drawer = app.page.locator('.log-drawer.open')
      assert.equal(await drawer.getAttribute('data-detail-id'), '42')
      await drawer.locator('[aria-busy="true"]').waitFor()
      assert.doesNotMatch(await drawer.innerText(), /stale-detail-sentinel/)
      assert.equal(await drawer.locator('.detail-error').count(), 0)
      next.release()
      await logReadSettled(app, path + '42')
      assert.equal(await drawer.getAttribute('data-detail-id'), '42')
      assert.equal(await drawer.locator('.detail-error').count(), 0)
      await closeLogDetail(app)
      mockLogs(app, fixture)
    }

    await logView(app, 'user')
    await logReadSettled(app, '/api/admin/request-logs')
    const collision = hold(app, 'GET /api/admin/request-logs/41', staleFails ? failure(500, 'stale-kind-sentinel') : json({
      log: userLog({ token_name: 'stale-kind-sentinel' }), execution_log_id: 999
    }))
    await logRow(app, 'user', 41).click()
    await bounded(collision.seen, 'colliding-ID entry arrival')
    await closeLogDetail(app)
    await logView(app, 'execution')
    await logReadSettled(app, '/api/admin/logs')
    await openLogDetail(app, 'execution', 41)
    collision.release()
    await logReadSettled(app, '/api/admin/request-logs/41')
    const executionDrawer = app.page.locator('.log-drawer.open')
    assert.equal(await executionDrawer.getAttribute('data-detail-kind'), 'execution')
    assert.equal(await executionDrawer.getAttribute('data-detail-id'), '41')
    assert.match(await executionDrawer.innerText(), /historical-41/)
    assert.doesNotMatch(await executionDrawer.innerText(), /stale-kind-sentinel/)
    assert.equal(await executionDrawer.locator('.detail-error').count(), 0)
    await closeLogDetail(app)
    mockLogs(app, fixture)

    await logView(app, 'user')
    await logReadSettled(app, '/api/admin/request-logs')
    await openLogDetail(app, 'user', 41)
    const linked = hold(app, 'GET /api/admin/logs/901', staleFails ? failure(500, 'stale-linked-sentinel') : json({
      log: executionLog({ id: 901, query: 'stale-linked-sentinel' }), calls: []
    }))
    const drawer = app.page.locator('.log-drawer.open')
    await drawer.getByRole('button', { name: '\u67e5\u770b\u6267\u884c\u65e5\u5fd7', exact: true }).click()
    await bounded(linked.seen, 'linked detail arrival')
    await drawer.getByRole('button', { name: '\u8fd4\u56de\u7528\u6237\u8bf7\u6c42', exact: true }).click()
    linked.release()
    await logReadSettled(app, '/api/admin/logs/901')
    await drawer.locator('[data-execution-state="linked"]').waitFor()
    assert.equal(await drawer.getAttribute('data-detail-kind'), 'user')
    assert.doesNotMatch(await drawer.innerText(), /stale-linked-sentinel/)
    assert.equal(await drawer.locator('.detail-error').count(), 0)
    await closeLogDetail(app)

    for (const boundary of ['close', 'tab', 'unmount']) {
      const old = hold(app, 'GET /api/admin/request-logs/41', staleFails ? failure(500, 'stale-closed-detail') : json({
        log: userLog({ token_name: 'stale-closed-detail' }), execution_log_id: 999
      }))
      await logRow(app, 'user', 41).click()
      await bounded(old.seen, boundary + ' detail arrival')
      await closeLogDetail(app)
      if (boundary === 'tab') {
        await logView(app, 'execution')
        await logReadSettled(app, '/api/admin/logs')
      } else if (boundary === 'unmount') await navigate(app, '/tokens')
      old.release()
      await logReadSettled(app, '/api/admin/request-logs/41')
      assert.equal(await app.page.locator('.log-drawer.open').count(), 0)
      assert.equal(await app.page.getByText('stale-closed-detail', { exact: true }).count(), 0)
      if (boundary === 'tab') {
        await logView(app, 'user')
        await logReadSettled(app, '/api/admin/request-logs')
      } else if (boundary === 'unmount') {
        await navigate(app, '/logs')
        await logReadSettled(app, '/api/admin/request-logs')
      }
    }
    await closeApp(app)
  }
}

/** Exercises real list/entry/linked UI 401 paths, preserving return URLs and never replaying expired reads. */
async function logExpiry(browser) {
  for (const targetKind of ['list', 'entry', 'linked']) {
    for (const reply of [failure(), { status: 401, contentType: 'text/html', body: '<h1>Unauthorized</h1>' }]) {
      const fixture = logFixtures()
      const app = await openLogs(browser, { fixture })
      const target = '/logs?probe=request-expiry#entry-41'
      await navigate(app, target)
      if (targetKind === 'linked') await openLogDetail(app, 'user', 41)
      const path = targetKind === 'list' ? '/api/admin/request-logs' : targetKind === 'entry' ? '/api/admin/request-logs/41' : '/api/admin/logs/901'
      const pending = hold(app, 'GET ' + path, reply)
      if (targetKind === 'list') await app.page.locator('.logs-actions button[title="\u5237\u65b0"]').click()
      else if (targetKind === 'entry') await logRow(app, 'user', 41).click()
      else await app.page.locator('.log-drawer.open').getByRole('button', { name: '\u67e5\u770b\u6267\u884c\u65e5\u5fd7', exact: true }).click()
      assert.equal((await bounded(pending.seen, 'log expiry arrival')).cookie, sessionA)
      const count = app.requests.filter((request) => request.key === 'GET ' + path && request.cookie === sessionA).length
      pending.release()
      await logReadSettled(app, path)
      await onLogin(app, target)
      await oneExpiry(app)
      assert.equal(await app.page.locator('.log-drawer.open').count(), 0)
      mockLogs(app, fixture)
      await login(app, target)
      await logReadSettled(app, '/api/admin/request-logs')
      await logRowsAre(app, 'user', fixture.entries.map((row) => row.id))
      assert.equal(app.requests.filter((request) => request.key === 'GET ' + path && request.cookie === sessionA).length, count)
      assert.equal(await app.page.locator('.log-drawer.open').count(), 0)
      await sessionIs(app, sessionB)
      await closeApp(app)
    }
  }
}

/** Invalidates real pending list/detail callbacks across auth revisions and preserves the newer same-ID selection. */
async function logSessionRaces(browser) {
  for (const targetKind of ['list', 'entry', 'execution']) {
    for (const status of [200, 500, 401]) {
      const fixture = logFixtures()
      const app = await openLogs(browser, { fixture })
      const path = targetKind === 'list' ? '/api/admin/request-logs' : targetKind === 'entry' ? '/api/admin/request-logs/41' : '/api/admin/logs/41'
      if (targetKind === 'execution') {
        await logView(app, 'execution')
        await logReadSettled(app, '/api/admin/logs')
      }
      const body = targetKind === 'list' ? { logs: [userLog({ id: 201, request_id: 'stale-session-entry' })] }
        : targetKind === 'entry' ? { log: userLog({ token_name: 'stale-session-entry' }), execution_log_id: 999 }
          : { log: executionLog({ query: 'stale-session-execution' }), calls: [] }
      const old = hold(app, 'GET ' + path, status === 200 ? json(body) : failure(status, 'stale-session-failure'))
      if (targetKind === 'list') await app.page.locator('.logs-actions button[title="\u5237\u65b0"]').click()
      else await logRow(app, targetKind === 'entry' ? 'user' : 'execution', 41).click()
      assert.equal((await bounded(old.seen, 'old-session log arrival')).cookie, sessionA)
      const oldIndex = await app.page.evaluate((path) => window.authProbe.logReads.filter((read) => read.path === path).length - 1, path)
      fixture.entries[0] = userLog({ token_name: 'current-session-token', execution_request_id: 'entry-41' })
      fixture.executions[0].log.query = 'current-session-execution'
      mockLogs(app, fixture)
      app.overrides.set('POST /api/admin/login', loginReply(sessionB))
      await app.page.evaluate(/** Uses the actual login and revision boundaries while keeping the log view mounted. */ async (credentials) => {
        const { api } = await import('/src/api/client.ts')
        window.authProbe.session.advanceRevision()
        await api.login(credentials.username, credentials.password)
        window.authProbe.session.advanceRevision()
      }, credentials)
      assert.equal(await app.page.locator('.log-drawer.open').count(), 0)
      await app.page.locator('.logs-actions button[title="\u5237\u65b0"]').click()
      await logReadSettled(app, targetKind === 'execution' ? '/api/admin/logs' : '/api/admin/request-logs')
      if (targetKind !== 'list') await openLogDetail(app, targetKind === 'entry' ? 'user' : 'execution', 41)
      const replacements = await app.page.evaluate(() => window.authProbe.replacements.length)
      old.release()
      await logReadSettled(app, path, oldIndex)
      if (targetKind === 'list') {
        await logRowsAre(app, 'user', fixture.entries.map((row) => row.id))
        assert.match(await logRow(app, 'user', 41).innerText(), /current-session-token/)
      } else {
        const drawer = app.page.locator('.log-drawer.open')
        assert.equal(await drawer.getAttribute('data-detail-id'), '41')
        assert.equal(await drawer.getAttribute('data-detail-kind'), targetKind === 'entry' ? 'user' : 'execution')
        assert.match(await drawer.innerText(), targetKind === 'entry' ? /current-session-token/ : /current-session-execution/)
        assert.doesNotMatch(await drawer.innerText(), /stale-session-/)
        assert.equal(await drawer.locator('.detail-error').count(), 0)
        await closeLogDetail(app)
      }
      assert.equal(await app.page.locator('.list-error, .login-card').count(), 0)
      assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), replacements)
      await sessionIs(app, sessionB)
      await closeApp(app)
    }
  }
}

/** Uses the real ten-second timer with held network work to prove pause, resume, no overlap and unmount cleanup. */
async function logAutoRefresh(browser) {
  const fixture = logFixtures()
  const app = await openLogs(browser, { fixture, clock: true })
  const toggle = app.page.locator('.logs-actions .el-switch')
  /** Counts actual routed reads so skipped ticks cannot masquerade as successful polling. */
  const count = (path) => app.requests.filter((request) => request.key === 'GET ' + path).length
  for (const kind of ['user', 'execution']) {
    await logView(app, kind)
    const path = kind === 'user' ? '/api/admin/request-logs' : '/api/admin/logs'
    await logReadSettled(app, path)
    const baseline = count(path)
    await app.page.clock.runFor(30000)
    assert.equal(count(path), baseline, 'Disabled auto-refresh sent a request')
    await toggle.click()
    await toggle.locator('[role="switch"][aria-checked="true"]').waitFor({ state: 'attached' })
    const polled = hold(app, 'GET ' + path, json({ logs: kind === 'user' ? fixture.entries : fixture.recent }))
    await app.page.clock.runFor(10000)
    await bounded(polled.seen, 'first poll arrival')
    assert.equal(count(path), baseline + 1)
    await app.page.clock.runFor(30000)
    assert.equal(count(path), baseline + 1, 'Polling overlapped a pending request')
    app.overrides.set('GET ' + path, json({ logs: kind === 'user' ? fixture.entries : fixture.recent }))
    polled.release()
    await logReadSettled(app, path)
    await openLogDetail(app, kind, 41)
    await app.page.clock.runFor(30000)
    assert.equal(count(path), baseline + 1, 'Polling continued while the detail drawer was open')
    await closeLogDetail(app)
    const resumed = hold(app, 'GET ' + path, failure(500, 'fixture poll unavailable'))
    await app.page.clock.runFor(10000)
    await bounded(resumed.seen, 'resumed poll arrival')
    assert.equal(count(path), baseline + 2)
    resumed.release()
    await logReadSettled(app, path)
    await app.page.locator('.list-error').getByText('fixture poll unavailable', { exact: true }).waitFor()
    await logRowsAre(app, kind, (kind === 'user' ? fixture.entries : fixture.recent).map((row) => row.id))
    app.overrides.set('GET ' + path, json({ logs: kind === 'user' ? fixture.entries : fixture.recent }))
    await app.page.clock.runFor(10000)
    await logReadSettled(app, path)
    assert.equal(count(path), baseline + 3, 'Polling did not recover after its handled error')
    await app.page.locator('.list-error').waitFor({ state: 'hidden' })
    await toggle.click()
    await toggle.locator('[role="switch"][aria-checked="false"]').waitFor({ state: 'attached' })
    await app.page.clock.runFor(30000)
    assert.equal(count(path), baseline + 3)
  }
  await logView(app, 'user')
  await logReadSettled(app, '/api/admin/request-logs')
  await toggle.click()
  const old = hold(app, 'GET /api/admin/request-logs', json({ logs: [userLog({ id: 201, request_id: 'unmounted-poll-sentinel' })] }))
  await app.page.clock.runFor(10000)
  await bounded(old.seen, 'unmounted poll arrival')
  const oldIndex = await app.page.evaluate(() => window.authProbe.logReads.filter((read) => read.path === '/api/admin/request-logs').length - 1)
  await navigate(app, '/tokens')
  const total = count('/api/admin/request-logs') + count('/api/admin/logs')
  await app.page.clock.runFor(30000)
  assert.equal(count('/api/admin/request-logs') + count('/api/admin/logs'), total, 'Unmounted page kept its interval')
  old.release()
  await logReadSettled(app, '/api/admin/request-logs', oldIndex)
  assert.equal(await app.page.locator('.logs-page, .log-drawer.open').count(), 0)
  mockLogs(app, fixture)
  await navigate(app, '/logs')
  await logReadSettled(app, '/api/admin/request-logs')
  await logRowsAre(app, 'user', fixture.entries.map((row) => row.id))
  assert.equal(await logRow(app, 'user', 201).count(), 0)
  await sessionIs(app, sessionA)
  await closeApp(app)
}

/** Runs the session and populated-log browser cases in order with separate finite deadlines. */
async function browserCases(browser) {
  await bounded(roundTrip(browser, desktop, 'desktop'), 'desktop round trip', 45000)
  await bounded(roundTrip(browser, mobile, 'mobile'), 'mobile round trip', 45000)
  await bounded(entryExpiry(browser), 'entry/error-body expiry', 45000)
  await bounded(parallelExpiry(browser), 'parallel expiry', 45000)
  await bounded(lateResponse(browser), 'old/new session ordering', 45000)
  await bounded(bodyRace(browser), 'error-body ordering', 45000)
  await bounded(wrongPassword(browser), 'wrong password', 45000)
  await bounded(otherErrors(browser), 'non-expiry errors', 45000)
  await bounded(manualLogout(browser), 'logout/migration', 60000)
  await bounded(logoutAcrossExpiry(browser), 'pending logout across expiry', 60000)
  await bounded(returnTargets(browser), 'return target matrix', 120000)
  await bounded(freshTabs(browser), 'fresh tabs and Cookie deletion', 60000)
  await bounded(probeErrors(browser), 'bootstrap and navigation probe errors', 90000)
  await bounded(pendingProbes(browser), 'pending and stale probes', 60000)
  await bounded(expiryProbeError(browser), 'expiry probe error ordering', 45000)
  await bounded(crossTabLateResponses(browser), 'cross-tab late responses', 60000)
  await bounded(populatedLogs(browser, desktop, 'desktop'), 'desktop populated log filters and layout', 60000)
  await bounded(populatedLogs(browser, mobile, 'mobile'), 'mobile populated log filters and layout', 60000)
  await bounded(logCorrelation(browser), 'exact log correlation and execution history', 60000)
  await bounded(logReadErrors(browser), 'log read errors and retry', 60000)
  await bounded(logListRaces(browser), 'log list generation races', 60000)
  await bounded(logDetailRaces(browser), 'log selection and detail races', 60000)
  await bounded(logExpiry(browser), 'log UI expiry boundaries', 60000)
  await bounded(logSessionRaces(browser), 'log callbacks across session revisions', 90000)
  await bounded(logAutoRefresh(browser), 'log polling lifecycle', 60000)
}

/**
 * Owns Vite/browser startup, diagnostics and bounded teardown for the CI matrix.
 * Rethrows the original primary value, even when falsy; cleanup-only failures
 * reject with AggregateError. Injected fixture operations exercise this same
 * lifecycle without processes or browsers, not a separate arbitration model.
 */
async function runBrowserHarness({
  startServer, chromium, runCases = browserCases, activeContexts = contexts,
  takeScreenshot = screenshot, processControl = process, wait = bounded, logger = console
}) {
  let output = ''
  let server
  let browserServer
  let browser
  let hasPrimary = false
  let primaryError
  const cleanupErrors = []
  const diagnosticErrors = []
  const ready = deferred()
  const exited = deferred()
  const startupError = deferred()
  const interrupted = deferred()
  /** Retains bounded raw startup output while matching ANSI-free readiness. */
  const onOutput = (chunk) => {
    output = `${output}${chunk}`.slice(-16000)
    if (stripVTControlCharacters(output).includes(`${origin}/`)) ready.resolve()
  }
  /** Settles the exit wait after Vite and its stdio handles have closed. */
  const onExit = () => exited.resolve()
  /** Preserves a spawn error for the startup race without an unhandled rejection. */
  const onStartupError = (error) => startupError.resolve(error)
  /** Records an interrupt so the browser matrix rejects through normal teardown. */
  const stop = () => interrupted.resolve()
  /** Signals the detached Vite process group; an already absent group is harmless. */
  const killServer = (signal) => {
    if (!server.pid) return
    try { processControl.kill(-server.pid, signal) } catch (error) { if (error?.code !== 'ESRCH') throw error }
  }
  /** Collects synchronous throws and rejections so later teardown still runs. */
  async function collect(errors, operation) {
    try {
      await operation()
      return true
    } catch (error) {
      errors.push(error)
      return false
    }
  }

  try {
    processControl.once('SIGINT', stop)
    processControl.once('SIGTERM', stop)
    processControl.once('SIGHUP', stop)
    server = startServer()
    server.once('close', onExit)
    server.once('error', onStartupError)
    for (const stream of [server.stdout, server.stderr]) stream.on('data', onOutput)
    await wait(Promise.race([
      ready.promise,
      startupError.promise.then(/** Rethrows the exact error emitted during spawn. */ (error) => { throw error }),
      exited.promise.then(/** Makes premature Vite exit a primary startup failure. */ () => { throw new Error(`Vite exited before readiness:\n${output}`) })
    ]), 'Vite readiness', 45000)
    browserServer = await chromium.launchServer({
      headless: true, host: '127.0.0.1', timeout: 30000,
      handleSIGINT: false, handleSIGTERM: false, handleSIGHUP: false
    })
    browser = await chromium.connect(browserServer.wsEndpoint(), { timeout: 30000 })
    await wait(Promise.race([
      runCases(browser),
      interrupted.promise.then(/** Converts process signals into the primary matrix failure. */ () => { throw new Error('Browser checks interrupted') })
    ]), 'browser matrix', 360000)
  } catch (error) {
    hasPrimary = true
    primaryError = error
    await collect(diagnosticErrors, /** Keeps startup logging from replacing the primary error. */ () => logger.error(output))
    if (browser) for (const context of activeContexts) {
      await collect(diagnosticErrors, /** Attempts each failed case's screenshot independently. */ async () => {
        const page = context.pages()[0]
        if (page && !page.isClosed()) await wait(takeScreenshot({ page }, 'failure'), 'failure screenshot', 5000)
      })
    }
  }

  for (const context of activeContexts) {
    await collect(cleanupErrors, /** Includes a synchronous context.close failure in the aggregate. */ () => wait(context.close(), 'context cleanup', 5000))
  }
  if (browser) await collect(cleanupErrors, /** Disconnects the client even if a context failed to close. */ () => wait(browser.close(), 'browser disconnect', 5000))
  if (browserServer) {
    const closed = await collect(cleanupErrors, /** Bounds graceful browser-process teardown. */ () => wait(browserServer.close(), 'browser process cleanup', 5000))
    if (!closed) {
      await collect(cleanupErrors, /** Retains forced browser termination after close failure. */ () => wait(browserServer.kill(), 'browser process kill', 5000))
    }
  }
  if (server) {
    await collect(cleanupErrors, /** Signal failures must not skip the exit wait or forced kill. */ () => killServer('SIGTERM'))
    try {
      await wait(exited.promise, 'Vite cleanup', 5000)
    } catch {
      await collect(cleanupErrors, /** Attempts SIGKILL even if SIGTERM failed synchronously. */ () => killServer('SIGKILL'))
      await collect(cleanupErrors, /** A failed forced-exit wait leaves cleanup incomplete. */ () => wait(exited.promise, 'Vite kill', 5000))
    }
    for (const stream of [server.stdout, server.stderr]) {
      await collect(cleanupErrors, /** Releases output observers after both exit attempts. */ () => stream.removeListener('data', onOutput))
    }
    await collect(cleanupErrors, /** Releases the one-shot exit observer even after incomplete cleanup. */ () => server.removeListener('close', onExit))
    await collect(cleanupErrors, /** Releases the startup-error observer after process teardown. */ () => server.removeListener('error', onStartupError))
  }
  for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
    await collect(cleanupErrors, /** Attempts every signal-listener removal independently. */ () => processControl.removeListener(signal, stop))
  }

  if (hasPrimary) {
    if (cleanupErrors.length) {
      await collect(diagnosticErrors, /** Reports cleanup separately without changing failure precedence. */ () => {
        return logger.error(new AggregateError(cleanupErrors, 'Browser/server cleanup failed'))
      })
    }
    if (diagnosticErrors.length) {
      await collect([], /** Diagnostic reporting itself remains best effort. */ () => {
        return logger.error(new AggregateError(diagnosticErrors, 'Browser failure diagnostics failed'))
      })
    }
    throw primaryError
  }
  if (cleanupErrors.length) throw new AggregateError(cleanupErrors, 'Browser/server cleanup failed')
  logger.log('Admin session expiry browser matrix passed')
}

/** Loads real browser dependencies only for the Actions CLI entry point. */
async function main() {
  assert.ok(process.env.FRONTEND_DIR, 'FRONTEND_DIR is required')
  assert.equal(process.env.VITE_API_BASE, '', 'Browser fixtures require an empty VITE_API_BASE')
  const { chromium } = require('playwright')
  const frontend = resolve(process.env.FRONTEND_DIR)
  await runBrowserHarness({
    chromium,
    /** Starts Vite in a detached group so teardown can reach its descendants. */
    startServer: () => spawn(process.execPath, [resolve(frontend, 'node_modules/vite/bin/vite.js'), '--host', '127.0.0.1', '--port', '4173', '--strictPort'], {
      cwd: frontend, detached: true, stdio: ['ignore', 'pipe', 'pipe'],
      env: { ...process.env, VITE_API_BASE: '', BROWSER: 'none', FORCE_COLOR: '0' }
    })
  })
}

module.exports = { runBrowserHarness }

if (require.main === module) {
  main().catch(/** Prints the selected primary or cleanup failure and exits nonzero. */ (error) => {
    try { console.error(error) } catch { /* A broken diagnostic sink must not prevent failure exit. */ }
    process.exit(1)
  })
}

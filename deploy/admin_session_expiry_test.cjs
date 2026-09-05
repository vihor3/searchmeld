'use strict'

if (process.env.GITHUB_ACTIONS !== 'true') {
  throw new Error('Admin session browser checks may run only in GitHub Actions')
}

const assert = require('node:assert/strict')
const { spawn } = require('node:child_process')
const { mkdir } = require('node:fs/promises')
const { resolve } = require('node:path')
const { stripVTControlCharacters } = require('node:util')
const { chromium } = require('playwright')

assert.ok(process.env.FRONTEND_DIR, 'FRONTEND_DIR is required')
assert.equal(process.env.VITE_API_BASE, '', 'Browser fixtures require an empty VITE_API_BASE')

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
const json = (body, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })
const failure = (status = 401, message = 'admin login required') => json({ error: { message, status } }, status)
const loginReply = (session) => ({
  ...json({ expires_at: '2099-01-01T00:00:00Z' }),
  issueSession: session,
  headers: { 'Set-Cookie': `${cookieName}=${session}; Path=/api/admin; HttpOnly; SameSite=Lax` }
})

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

function observeErrors(app, page) {
  page.on('pageerror', (error) => {
    if (app.expectedErrors.has(error.message)) app.diagnostics.push(error.message)
    else app.problems.push(`pageerror: ${error.message}`)
  })
  page.on('console', (message) => {
    if (message.type() !== 'error') return
    if (message.text().startsWith('Failed to load resource:') && app.requests.some((request) => request.failed && request.url === message.location().url)) {
      app.diagnostics.push(message.text())
    } else {
      app.problems.push(`console: ${message.text()}`)
    }
  })
}

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
  await context.routeWebSocket('**/*', async (socket) => {
    if (new URL(socket.url()).origin === origin.replace('http:', 'ws:')) socket.connectToServer()
    else {
      app.problems.push(`Unexpected external WebSocket: ${socket.url()}`)
      await socket.close()
    }
  })
  await context.route('**/*', async (route) => {
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
      await route.abort().catch(() => {})
    }
  })

  // Seed once on an inert same-origin document, never from a reload-time init script.
  await page.goto(`${origin}/__session_fixture__`)
  await page.evaluate(({ session, legacy, tokenKeys }) => {
    if (legacy) {
      for (const key of tokenKeys) sessionStorage.setItem(key, session || 'synthetic-legacy-session')
      for (const key of tokenKeys) localStorage.setItem(key, 'synthetic-old-local-copy')
    }
  }, { session, legacy, tokenKeys })
  return app
}

async function observe(app) {
  await app.page.evaluate(async () => {
    const [{ default: router }, { useSessionStore }, { apiFetch }] = await Promise.all([
      import('/src/router/index.ts'), import('/src/stores/session.ts'), import('/src/api/client.ts')
    ])
    window.authProbe = { router, session: useSessionStore(), apiFetch, transitions: [], replacements: [], checkCalls: 0 }
    window.authProbe.session.$onAction(({ name }) => { if (name === 'check') window.authProbe.checkCalls += 1 })
    router.afterEach((to, _from, failure) => { if (!failure) window.authProbe.transitions.push(to.fullPath) })
    const replace = router.replace.bind(router)
    router.replace = (to) => {
      window.authProbe.replacements.push(router.resolve(to).fullPath)
      return replace(to)
    }
  })
}

async function at(app, path) {
  const expected = new URL(path, origin).href
  await app.page.waitForURL(expected)
  await app.page.waitForFunction((expected) => new URL(window.authProbe.router.currentRoute.value.fullPath, location.origin).href === expected, expected)
  if (new URL(path, origin).pathname === '/logs') {
    // Element Plus puts the switch role on a zero-sized input; click its visible wrapper.
    const toggle = app.page.locator('.logs-actions .el-switch')
    await toggle.waitFor()
    if (await toggle.locator('[role="switch"]').getAttribute('aria-checked') === 'true') await toggle.click()
    await toggle.locator('[role="switch"][aria-checked="false"]').waitFor({ state: 'attached' })
  }
}

async function visit(app, path) {
  await app.page.goto(`${origin}${path}`)
  await observe(app)
}

async function navigate(app, path) {
  await app.page.evaluate((path) => window.authProbe.router.push(path), path)
  await at(app, path)
}

function hold(app, key, reply) {
  const seen = deferred()
  const release = deferred()
  app.overrides.set(key, async (request) => {
    app.overrides.delete(key)
    seen.resolve(request)
    await release.promise
    return reply
  })
  return { seen: seen.promise, release: release.resolve }
}

async function storageClean(app) {
  const state = await app.page.evaluate(() => ({
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

async function legacyCopies(app) {
  await app.page.evaluate(({ keys, session }) => {
    for (const key of keys) sessionStorage.setItem(key, session)
    for (const key of keys) localStorage.setItem(key, session)
  }, { keys: tokenKeys, session: sessionA })
}

async function onLogin(app, redirect) {
  await app.page.locator('.login-card').waitFor()
  const viewport = app.page.viewportSize()
  assert.ok(viewport, 'Login layout requires an explicit browser viewport')
  const layout = await app.page.evaluate(() => ({
    viewport: document.documentElement.clientWidth,
    windowWidth: window.innerWidth,
    elements: ['html', 'body', '#app', '.login-page', '.login-card'].map((selector) => {
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

async function startRequests(app, requests) {
  await app.page.evaluate((requests) => {
    window.authProbe.pending = Promise.allSettled(requests.map(({ path, options }) => window.authProbe.apiFetch(path, options)))
  }, requests.map((request) => typeof request === 'string' ? { path: request } : request))
}

async function rejected(app, messages) {
  const results = await bounded(app.page.evaluate(async () => (await window.authProbe.pending).map((result) => ({
    status: result.status, message: result.status === 'rejected' ? result.reason.message : undefined
  }))), 'request rejection')
  assert.deepEqual(results, messages.map((message) => ({ status: 'rejected', message })))
}

async function oneExpiry(app) {
  const counts = await app.page.evaluate(() => ({
    replacements: window.authProbe.replacements.length,
    logins: window.authProbe.transitions.filter((path) => path.startsWith('/login?')).length
  }))
  assert.deepEqual(counts, { replacements: 1, logins: 1 })
}

async function screenshot(app, name) {
  if (!process.env.ARTIFACT_DIR) return
  await mkdir(process.env.ARTIFACT_DIR, { recursive: true })
  await app.page.screenshot({ path: resolve(process.env.ARTIFACT_DIR, `${name}.png`), fullPage: true })
}

async function closeApp(app) {
  await bounded(app.context.close(), 'context cleanup', 5000)
  contexts.delete(app.context)
  assert.deepEqual(app.problems, [], 'Unexpected browser/network errors')
  console.log(`Browser case completed (${app.diagnostics.length} intentional error diagnostics)`)
}

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
  await app.page.evaluate(() => { window.authProbe.replacements = []; window.authProbe.transitions = [] })
  await legacyCopies(app)
  app.expectedErrors.add('admin login required')
  const refresh = hold(app, 'GET /api/admin/logs', failure())
  await app.page.locator('.logs-actions button[title="\u5237\u65b0"]').click()
  assert.equal((await refresh.seen).cookie, sessionA)
  refresh.release()
  await onLogin(app, target)
  await oneExpiry(app)
  await app.page.waitForFunction(() => document.querySelector('.login-logo')?.naturalWidth > 0)
  await screenshot(app, `${name}-expired`)
  await login(app, target)
  assert.equal(app.requests.filter((request) => request.key === 'GET /api/admin/logs' && request.cookie === sessionA).length, 3)
  await app.page.locator('.logs-actions button[title="\u5237\u65b0"]:not(.is-loading)').waitFor()
  await screenshot(app, `${name}-returned`)
  await app.page.goBack()
  await at(app, '/tokens')
  await sessionIs(app, sessionB)
  await closeApp(app)
}

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

async function bodyRace(browser) {
  const app = await openApp(browser, { session: sessionA })
  await visit(app, '/tokens')
  await app.page.locator('.token-table').waitFor()
  app.overrides.set('GET /api/admin/logs', failure())
  // Split the response headers/body await boundary without replacing apiFetch's auth logic.
  await app.page.evaluate(() => {
    const fetch = window.fetch
    window.fetch = async (...args) => {
      const response = await fetch(...args)
      if (String(args[0]).includes('probe=body-race')) {
        window.fetch = fetch
        const read = response.json.bind(response)
        response.json = async () => {
          window.authProbe.bodyStarted = true
          await new Promise((resolve) => { window.authProbe.releaseBody = resolve })
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

async function otherErrors(browser) {
  const app = await openApp(browser, { session: sessionA })
  const target = '/playground?probe=errors#search'
  await visit(app, target)
  await app.page.locator('.search-input input').waitFor()
  const cases = [
    ...[403, 429, 500].map((status) => ({ path: '/api/admin/logs', reply: failure(status, `fixture-${status}`), message: `fixture-${status}` })),
    { path: '/api/admin/logs', reply: { abort: true }, message: 'Failed to fetch' },
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

async function logoutAcrossExpiry(browser) {
  for (const status of [200, 401, 'network']) {
    for (const reauthenticate of [false, true]) {
      const app = await openApp(browser, { session: sessionA })
      const target = '/tokens?probe=pending-logout#list'
      await visit(app, target)
      await app.page.locator('.token-table').waitFor()
      await app.page.evaluate(async () => {
        const { api } = await import('/src/api/client.ts')
        const logout = api.logout
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
      const message = await bounded(app.page.evaluate(async () => {
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
    await app.page.evaluate((redirect) => window.authProbe.router.replace({ path: '/login', query: { redirect } }), redirect)
    await app.page.locator('.login-card').waitFor()
    await login(app, expected)
    // The authenticated-login guard must use the same validation as form submission.
    await app.page.evaluate((redirect) => window.authProbe.router.push({ path: '/login', query: { redirect } }), redirect)
    await at(app, expected)
    await sessionIs(app, sessionB)
  }
  await closeApp(app)
}

async function newTab(app, path) {
  const page = await app.context.newPage()
  observeErrors(app, page)
  const tab = { ...app, page }
  await visit(tab, path)
  return tab
}

async function freshTabs(browser) {
  const target = '/tokens?probe=shared#list'
  const app = await openApp(browser, { legacy: true })
  await visit(app, target)
  await onLogin(app, target)
  await login(app, target, sessionA)
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
    await app.page.evaluate(() => { window.authProbe.navigation = window.authProbe.router.push('/providers?probe=retry#keys') })
    await probe.seen
    probe.release()
    await app.page.evaluate(() => window.authProbe.navigation)
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

async function pendingProbes(browser) {
  const app = await openApp(browser, { session: sessionA })
  await visit(app, '/tokens')
  await app.page.locator('.token-table').waitFor()
  const count = app.requests.filter((request) => request.key === 'GET /api/admin/me').length
  const checkCalls = await app.page.evaluate(() => window.authProbe.checkCalls)
  const pending = hold(app, 'GET /api/admin/me', json({ username: credentials.username }))
  await app.page.evaluate(() => { window.authProbe.firstNavigation = window.authProbe.router.push('/providers') })
  await pending.seen
  await app.page.evaluate(() => { window.authProbe.secondNavigation = window.authProbe.router.push('/logs') })
  await app.page.waitForFunction((minimum) => window.authProbe.checkCalls >= minimum, checkCalls + 2)
  assert.equal(app.requests.filter((request) => request.key === 'GET /api/admin/me').length, count + 1)
  pending.release()
  await app.page.evaluate(() => Promise.all([window.authProbe.firstNavigation, window.authProbe.secondNavigation]))
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
    await app.page.evaluate(() => { window.authProbe.oldNavigation = window.authProbe.router.push('/providers') })
    await old.seen
    app.overrides.set('POST /api/admin/login', loginReply(sessionB))
    // Exercise the same in-tab auth-operation boundary while the old guard is pending.
    await app.page.evaluate(async (credentials) => {
      const { api } = await import('/src/api/client.ts')
      window.authProbe.session.advanceRevision()
      await api.login(credentials.username, credentials.password)
      window.authProbe.session.advanceRevision()
      await window.authProbe.router.replace('/tokens?probe=new-session')
    }, credentials)
    await at(app, '/tokens?probe=new-session')
    old.release()
    await app.page.evaluate(() => window.authProbe.oldNavigation)
    await at(app, '/tokens?probe=new-session')
    await sessionIs(app, sessionB)
    assert.equal(await app.page.locator('.session-error, .login-card').count(), 0)
    assert.deepEqual(await app.page.evaluate(() => window.authProbe.session.profile), { username: credentials.username })
    await closeApp(app)
  }
}

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
    await app.page.evaluate(() => {
      window.authProbe.loginShown = false
      window.authProbe.loginObserver = new MutationObserver(() => {
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
    assert.equal(await app.page.evaluate(() => {
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

async function main() {
  const frontend = resolve(process.env.FRONTEND_DIR)
  const server = spawn(process.execPath, [resolve(frontend, 'node_modules/vite/bin/vite.js'), '--host', '127.0.0.1', '--port', '4173', '--strictPort'], {
    cwd: frontend, detached: true, stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, VITE_API_BASE: '', BROWSER: 'none', FORCE_COLOR: '0' }
  })
  let output = ''
  let browserServer
  let browser
  const ready = deferred()
  const exited = new Promise((done) => server.once('close', done))
  const startupError = new Promise((_, reject) => server.once('error', reject))
  for (const stream of [server.stdout, server.stderr]) stream.on('data', (chunk) => {
    output = `${output}${chunk}`.slice(-16000)
    if (stripVTControlCharacters(output).includes(`${origin}/`)) ready.resolve()
  })
  const interrupted = deferred()
  const stop = () => interrupted.resolve()
  process.once('SIGINT', stop)
  process.once('SIGTERM', stop)
  process.once('SIGHUP', stop)
  const killServer = (signal) => {
    if (!server.pid) return
    try { process.kill(-server.pid, signal) } catch (error) { if (error.code !== 'ESRCH') throw error }
  }
  const cleanupErrors = []
  try {
    await bounded(Promise.race([ready.promise, startupError, exited.then(() => { throw new Error(`Vite exited before readiness:\n${output}`) })]), 'Vite readiness', 45000)
    browserServer = await chromium.launchServer({
      headless: true, host: '127.0.0.1', timeout: 30000,
      handleSIGINT: false, handleSIGTERM: false, handleSIGHUP: false
    })
    browser = await chromium.connect(browserServer.wsEndpoint(), { timeout: 30000 })
    const cases = async () => {
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
    }
    await bounded(Promise.race([cases(), interrupted.promise.then(() => { throw new Error('Browser checks interrupted') })]), 'browser matrix', 360000)
  } catch (error) {
    console.error(output)
    if (browser) for (const context of contexts) {
      const page = context.pages()[0]
      if (page && !page.isClosed()) await bounded(screenshot({ page }, 'failure'), 'failure screenshot', 5000).catch(() => {})
    }
    throw error
  } finally {
    for (const context of contexts) {
      await bounded(context.close(), 'context cleanup', 5000).catch((error) => cleanupErrors.push(error))
    }
    if (browser) await bounded(browser.close(), 'browser disconnect', 5000).catch((error) => cleanupErrors.push(error))
    if (browserServer) {
      try {
        await bounded(browserServer.close(), 'browser process cleanup', 5000)
      } catch (error) {
        cleanupErrors.push(error)
        await bounded(browserServer.kill(), 'browser process kill', 5000).catch((error) => cleanupErrors.push(error))
      }
    }
    killServer('SIGTERM')
    try {
      await bounded(exited, 'Vite cleanup', 5000)
    } catch {
      killServer('SIGKILL')
      await bounded(exited, 'Vite kill', 5000).catch((error) => cleanupErrors.push(error))
    }
    process.removeListener('SIGINT', stop)
    process.removeListener('SIGTERM', stop)
    process.removeListener('SIGHUP', stop)
    if (cleanupErrors.length) throw new AggregateError(cleanupErrors, 'Browser/server cleanup failed')
  }
  console.log('Admin session expiry browser matrix passed')
}

main().catch((error) => { console.error(error); process.exit(1) })

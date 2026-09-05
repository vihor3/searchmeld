'use strict'

if (process.env.GITHUB_ACTIONS !== 'true') {
  throw new Error('Admin session browser checks may run only in GitHub Actions')
}

const assert = require('node:assert/strict')
const { spawn } = require('node:child_process')
const { mkdir } = require('node:fs/promises')
const { resolve } = require('node:path')
const { chromium } = require('playwright')

assert.ok(process.env.FRONTEND_DIR, 'FRONTEND_DIR is required')
assert.equal(process.env.VITE_API_BASE, '', 'Browser fixtures require an empty VITE_API_BASE')

const origin = 'http://127.0.0.1:4173'
const tokenA = 'synthetic-admin-session-A'
const tokenB = 'synthetic-admin-session-B'
const credentials = { username: 'fixture-operator', password: 'synthetic-password-not-persisted' }
const searchQuery = 'synthetic-search-not-replayed'
const tokenKeys = ['searchmeld-admin-token', 'one-search-admin-token']
const desktop = { width: 1440, height: 1000 }
const mobile = { width: 390, height: 844 }
const contexts = new Set()
const json = (body, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })
const failure = (status = 401, message = 'admin login required') => json({ error: { message, status } }, status)
const loginReply = (token) => json({ token, expires_at: '2099-01-01T00:00:00Z' })

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

async function openApp(browser, { token = '', legacy = false, viewport = desktop } = {}) {
  const context = await browser.newContext({ viewport, serviceWorkers: 'block', reducedMotion: 'reduce' })
  contexts.add(context)
  context.setDefaultTimeout(15000)
  const page = await context.newPage()
  const app = { context, page, requests: [], overrides: new Map(), problems: [], expectedErrors: new Set(), diagnostics: [] }
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
    'POST /api/admin/logout': json({ ok: true })
  }))

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
        const handler = app.overrides.get(key) || defaults.get(key)
        assert.ok(handler, `Unexpected API request: ${key}`)
        const entry = { key, url: request.url(), token: request.headers().authorization || '', body: request.postData(), failed: false }
        app.requests.push(entry)
        const reply = typeof handler === 'function' ? await handler(entry) : handler
        entry.failed = Boolean(reply.abort || reply.status >= 400)
        if (reply.abort) await route.abort('failed')
        else await route.fulfill(reply)
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
  await page.evaluate(({ token, legacy, tokenKeys }) => {
    if (token) {
      sessionStorage.setItem(tokenKeys[legacy ? 1 : 0], token)
      for (const key of tokenKeys) localStorage.setItem(key, 'synthetic-old-local-copy')
    }
  }, { token, legacy, tokenKeys })
  return app
}

async function observe(app) {
  await app.page.evaluate(async () => {
    const [{ default: router }, { useSessionStore }, { apiFetch }] = await Promise.all([
      import('/src/router/index.ts'), import('/src/stores/session.ts'), import('/src/api/client.ts')
    ])
    await router.isReady()
    window.authProbe = { router, session: useSessionStore(), apiFetch, transitions: [], replacements: [] }
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

async function sessionIs(app, token) {
  const state = await app.page.evaluate(() => ({
    token: window.authProbe.session.token,
    session: { ...sessionStorage }, local: { ...localStorage }
  }))
  assert.equal(state.token, token)
  assert.equal(state.session[tokenKeys[0]], token || undefined)
  assert.equal(state.session[tokenKeys[1]], undefined)
  for (const key of tokenKeys) assert.equal(state.local[key], undefined)
  for (const secret of [credentials.username, credentials.password, searchQuery]) {
    assert.ok(!JSON.stringify([state.session, state.local]).includes(secret), 'Form/request data was persisted')
  }
}

async function legacyCopies(app) {
  await app.page.evaluate((keys) => {
    sessionStorage.setItem(keys[1], 'synthetic-legacy-copy')
    for (const key of keys) localStorage.setItem(key, 'synthetic-local-copy')
  }, tokenKeys)
}

async function onLogin(app, redirect) {
  await app.page.locator('.login-card').waitFor()
  const url = new URL(app.page.url())
  assert.equal(url.pathname, '/login')
  assert.equal(url.searchParams.get('redirect'), redirect)
  await sessionIs(app, '')
}

async function login(app, target, token = tokenB) {
  const response = hold(app, 'POST /api/admin/login', loginReply(token))
  const inputs = app.page.locator('.login-card input')
  await inputs.nth(0).fill(credentials.username)
  await inputs.nth(1).fill(credentials.password)
  const button = app.page.locator('.login-card .el-button')
  await button.click()
  const request = await response.seen
  assert.deepEqual(JSON.parse(request.body), credentials)
  assert.equal(await button.isDisabled(), true)
  response.release()
  await at(app, target)
  await sessionIs(app, token)
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
  await login(app, target, tokenA)
  await navigate(app, '/tokens')
  await navigate(app, target)
  await app.page.locator('.logs-actions button[title="\u5237\u65b0"]:not(.is-loading)').waitFor()
  await app.page.evaluate(() => { window.authProbe.replacements = []; window.authProbe.transitions = [] })
  await legacyCopies(app)
  app.expectedErrors.add('admin login required')
  const refresh = hold(app, 'GET /api/admin/logs', failure())
  await app.page.locator('.logs-actions button[title="\u5237\u65b0"]').click()
  assert.equal((await refresh.seen).token, `Bearer ${tokenA}`)
  refresh.release()
  await onLogin(app, target)
  await oneExpiry(app)
  await app.page.waitForFunction(() => document.querySelector('.login-logo')?.naturalWidth > 0)
  await screenshot(app, `${name}-expired`)
  await login(app, target)
  assert.equal(app.requests.filter((request) => request.key === 'GET /api/admin/logs' && request.token === `Bearer ${tokenA}`).length, 3)
  await app.page.locator('.logs-actions button[title="\u5237\u65b0"]:not(.is-loading)').waitFor()
  await screenshot(app, `${name}-returned`)
  await app.page.goBack()
  await at(app, '/tokens')
  await sessionIs(app, tokenB)
  await closeApp(app)
}

async function entryExpiry(browser) {
  for (const reply of [failure(), { status: 401, body: '' }, { status: 401, contentType: 'text/html', body: '<h1>Unauthorized</h1>' }]) {
    const app = await openApp(browser, { token: tokenA })
    app.expectedErrors.add('admin login required').add('Unauthorized')
    const response = hold(app, 'GET /api/admin/tokens', reply)
    const target = '/tokens?probe=entry#list'
    await visit(app, target)
    assert.equal((await response.seen).token, `Bearer ${tokenA}`)
    await legacyCopies(app)
    response.release()
    await onLogin(app, target)
    await oneExpiry(app)
    await app.page.reload()
    await observe(app)
    await onLogin(app, target)
    await login(app, target)
    await app.page.locator('.token-table').waitFor()
    assert.deepEqual(app.requests.filter((request) => request.key === 'GET /api/admin/tokens').map((request) => request.token), [`Bearer ${tokenA}`, `Bearer ${tokenB}`])
    await closeApp(app)
  }
}

async function parallelExpiry(browser) {
  for (const staggered of [false, true]) {
    const app = await openApp(browser, { token: tokenA })
    const target = '/providers?probe=parallel#keys'
    await visit(app, target)
    await app.page.locator('.provider-grid').waitFor()
    const providers = hold(app, 'GET /api/admin/providers', failure())
    const keys = hold(app, 'GET /api/admin/keys', failure())
    await startRequests(app, ['/api/admin/providers', '/api/admin/keys'])
    const requests = await Promise.all([providers.seen, keys.seen])
    assert.deepEqual(requests.map((request) => request.token), [`Bearer ${tokenA}`, `Bearer ${tokenA}`])
    providers.release()
    if (staggered) await onLogin(app, target)
    keys.release()
    await rejected(app, ['admin login required', 'admin login required'])
    await onLogin(app, target)
    await oneExpiry(app)
    await login(app, target)
    await app.page.locator('.provider-grid').waitFor()
    for (const key of ['GET /api/admin/providers', 'GET /api/admin/keys']) {
      assert.equal(app.requests.filter((request) => request.key === key && request.token === `Bearer ${tokenA}`).length, 2)
    }
    await closeApp(app)
  }
}

async function lateResponse(browser) {
  const app = await openApp(browser, { token: tokenA })
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
  await sessionIs(app, tokenB)
  assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 2)
  const expireB = hold(app, 'GET /api/admin/keys', failure())
  await startRequests(app, ['/api/admin/keys?probe=current-B'])
  assert.equal((await expireB.seen).token, `Bearer ${tokenB}`)
  expireB.release()
  await rejected(app, ['admin login required'])
  await onLogin(app, current)
  assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 3)
  await closeApp(app)
}

async function bodyRace(browser) {
  const app = await openApp(browser, { token: tokenA })
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
  await sessionIs(app, tokenB)
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
  assert.equal((await badLogin.seen).token, '')
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
  const app = await openApp(browser, { token: tokenA })
  const target = '/playground?probe=errors#search'
  await visit(app, target)
  await app.page.locator('.search-input input').waitFor()
  const cases = [
    ...[403, 429, 500].map((status) => ({ path: '/api/admin/logs', reply: failure(status, `fixture-${status}`), message: `fixture-${status}` })),
    { path: '/api/admin/logs', reply: { abort: true }, message: 'Failed to fetch' },
    { path: '/v1/search', reply: failure(401, 'API token rejected'), message: 'API token rejected', options: { method: 'POST', body: JSON.stringify({ query: searchQuery }) } },
    { path: '/api/admin/login?probe=existing-session', reply: failure(401, 'credentials rejected'), message: 'credentials rejected', options: { method: 'POST', body: JSON.stringify(credentials) } }
  ]
  for (const test of cases) {
    const response = hold(app, `${test.options?.method || 'GET'} ${new URL(test.path, origin).pathname}`, test.reply)
    await startRequests(app, [test])
    assert.equal((await response.seen).token, `Bearer ${tokenA}`)
    response.release()
    await rejected(app, [test.message])
    await sessionIs(app, tokenA)
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
      await sessionIs(app, tokenA)
    } else {
      await onLogin(app, target)
      await oneExpiry(app)
      await login(app, target)
      await app.page.locator('.search-input input').waitFor()
      assert.equal(await app.page.locator('.search-input input').inputValue(), '')
    }
  }
  assert.equal(app.requests.filter((request) => request.key === 'POST /api/admin/playground/search').length, 2)
  await app.page.evaluate(async () => { window.authProbe.session.logout(); await window.authProbe.router.replace('/login') })
  const count = await app.page.evaluate(() => window.authProbe.replacements.length)
  const empty = hold(app, 'GET /api/admin/providers', failure())
  await startRequests(app, ['/api/admin/providers'])
  assert.equal((await empty.seen).token, '')
  empty.release()
  await rejected(app, ['admin login required'])
  await onLogin(app, null)
  assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), count)
  await closeApp(app)
}

async function manualLogout(browser) {
  for (const status of [200, 401, 403, 500, 'network']) {
    const app = await openApp(browser, { token: tokenA, legacy: true })
    app.expectedErrors.add('logout rejected').add('Failed to fetch')
    await visit(app, '/providers')
    await app.page.locator('.provider-grid').waitFor()
    await sessionIs(app, tokenA)
    const late = hold(app, 'GET /api/admin/keys', failure())
    await startRequests(app, ['/api/admin/keys?probe=after-logout'])
    await late.seen
    await legacyCopies(app)
    const reply = status === 'network' ? { abort: true } : status === 200 ? json({ ok: true }) : failure(status, 'logout rejected')
    const logout = hold(app, 'POST /api/admin/logout', reply)
    await app.page.locator('.float-logout').click()
    assert.equal((await logout.seen).token, `Bearer ${tokenA}`)
    logout.release()
    await onLogin(app, null)
    late.release()
    await rejected(app, ['admin login required'])
    await onLogin(app, null)
    assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), 0)
    await app.page.reload()
    await observe(app)
    await onLogin(app, null)
    if (status === 200) {
      await app.page.evaluate(() => window.authProbe.router.push('/tokens?probe=guard#later'))
      await onLogin(app, '/tokens?probe=guard#later')
      await app.page.evaluate(() => window.authProbe.router.replace('/login'))
    }
    await login(app, '/playground')
    assert.equal(app.requests.filter((request) => request.key === 'POST /api/admin/logout').length, 1)
    assert.equal(app.requests.filter((request) => request.url.includes('probe=after-logout')).length, 1)
    await closeApp(app)
  }
}

async function logoutAcrossExpiry(browser) {
  for (const status of [200, 401, 'network']) {
    for (const reauthenticate of [false, true]) {
      const app = await openApp(browser, { token: tokenA })
      app.expectedErrors.add('logout rejected').add('Failed to fetch')
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

      const reply = status === 'network' ? { abort: true } : status === 200 ? json({ ok: true }) : failure(status, 'logout rejected')
      const logout = hold(app, 'POST /api/admin/logout', reply)
      await app.page.locator('.float-logout').click()
      assert.equal((await logout.seen).token, `Bearer ${tokenA}`)
      const expire = hold(app, 'GET /api/admin/keys', failure())
      await startRequests(app, ['/api/admin/keys?probe=logout-expiry'])
      assert.equal((await expire.seen).token, `Bearer ${tokenA}`)
      expire.release()
      await rejected(app, ['admin login required'])
      await onLogin(app, target)
      await oneExpiry(app)
      if (reauthenticate) {
        await login(app, target)
        await app.page.locator('.token-table').waitFor()
      } else {
        await legacyCopies(app)
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
        await sessionIs(app, tokenB)
        await at(app, target)
        assert.deepEqual(await app.page.evaluate(() => window.authProbe.transitions), transitions)
      } else {
        await at(app, '/login')
        await onLogin(app, null)
        assert.deepEqual(await app.page.evaluate(() => window.authProbe.transitions), [...transitions, '/login'])
      }
      assert.equal(await app.page.evaluate(() => window.authProbe.replacements.length), reauthenticate ? 2 : 1)
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
    await app.page.evaluate(async (redirect) => {
      window.authProbe.session.logout()
      await window.authProbe.router.replace({ path: '/login', query: { redirect } })
    }, redirect)
    await login(app, expected)
    // The authenticated-login guard must use the same validation as form submission.
    await app.page.evaluate((redirect) => window.authProbe.router.push({ path: '/login', query: { redirect } }), redirect)
    await at(app, expected)
    await sessionIs(app, tokenB)
  }
  await closeApp(app)
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
    if (output.includes(`${origin}/`)) ready.resolve()
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
      await bounded(lateResponse(browser), 'old/new token ordering', 45000)
      await bounded(bodyRace(browser), 'error-body ordering', 45000)
      await bounded(wrongPassword(browser), 'wrong password', 45000)
      await bounded(otherErrors(browser), 'non-expiry errors', 45000)
      await bounded(manualLogout(browser), 'logout/migration', 60000)
      await bounded(logoutAcrossExpiry(browser), 'pending logout across expiry', 60000)
      await bounded(returnTargets(browser), 'return target matrix', 120000)
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

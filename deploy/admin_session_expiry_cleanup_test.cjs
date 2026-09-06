'use strict'

if (process.env.GITHUB_ACTIONS !== 'true') {
  throw new Error('Admin session cleanup checks may run only in GitHub Actions')
}

const assert = require('node:assert/strict')
const { EventEmitter } = require('node:events')
const test = require('node:test')
const { runBrowserHarness } = require('./admin_session_expiry_test.cjs')

/**
 * Supplies synthetic Vite, browser and diagnostic operations to the real harness.
 * Faults distinguish synchronous throws from rejected promises, including falsy
 * values. No process, browser, filesystem or network operation is started.
 */
function harnessFixture({ faults = {}, startup = 'ready', startupError, interrupt = false, exitOnTerm = true } = {}) {
  const operations = []
  const errors = []
  const logs = []
  const activeContexts = new Set()
  const processControl = new EventEmitter()
  const server = new EventEmitter()
  server.pid = 12345
  server.stdout = new EventEmitter()
  server.stderr = new EventEmitter()
  let exited = false

  /** Records a reached operation before returning or raising its configured fault. */
  function perform(name, value) {
    operations.push(name)
    const fault = faults[name]
    if (fault) {
      if (fault.async) return Promise.reject(fault.error)
      throw fault.error
    }
    return value
  }

  /** Emits Vite's close event once, allowing the actual exit promise to settle. */
  function exitServer() {
    if (exited) return
    exited = true
    server.emit('close', 0)
  }

  /** Records detached-group signals and simulates graceful or forced Vite exit. */
  processControl.kill = (pid, signal) => {
    assert.equal(pid, -server.pid)
    perform(`Vite ${signal}`)
    if (signal === 'SIGKILL' || exitOnTerm) exitServer()
  }
  /** Records each signal-listener disposal; a failed removal cannot hide later attempts. */
  processControl.removeListener = (signal, listener) => {
    perform(`remove ${signal}`)
    return EventEmitter.prototype.removeListener.call(processControl, signal, listener)
  }

  const fixtureContexts = ['first', 'second'].map(/** Creates separately observable context and screenshot cleanup targets. */ (name) => {
    const page = {
      name,
      /** Leaves a synthetic page available for failure screenshot attempts. */
      isClosed: () => perform(`page ${name} isClosed`, false)
    }
    return {
      /** Models synchronous page enumeration failures before screenshot capture. */
      pages: () => perform(`pages ${name}`, [page]),
      /** Models both close throws and rejections without real browser handles. */
      close: () => perform(`context ${name}`, Promise.resolve())
    }
  })
  const browser = {
    /** Records browser-client teardown independently of context disposal. */
    close: () => perform('browser disconnect', Promise.resolve())
  }
  const browserServer = {
    /** Gives the connection fixture a fixed endpoint that is never contacted. */
    wsEndpoint: () => 'ws://fixture.invalid/browser',
    /** Can fail before the harness reaches its forced-termination fallback. */
    close: () => perform('browser close', Promise.resolve()),
    /** Records forced termination separately from graceful browser closure. */
    kill: () => perform('browser kill', Promise.resolve())
  }

  const options = {
    activeContexts,
    processControl,
    /** Emits readiness, a spawn error or premature exit after listeners attach. */
    startServer() {
      perform('startServer')
      queueMicrotask(/** Exercises the actual event-driven Vite startup race. */ () => {
        if (startup === 'error') server.emit('error', startupError)
        else if (startup === 'closed') exitServer()
        else server.stdout.emit('data', 'Local: http://127.0.0.1:4173/\n')
      })
      return server
    },
    chromium: {
      /** Checks the real launch deadline and disabled automatic signal handlers. */
      launchServer(options) {
        assert.deepEqual(options, {
          headless: true, host: '127.0.0.1', timeout: 30000,
          handleSIGINT: false, handleSIGTERM: false, handleSIGHUP: false
        })
        return perform('launchServer', browserServer)
      },
      /** Checks that connection still has its original finite deadline. */
      connect(endpoint, options) {
        assert.equal(endpoint, 'ws://fixture.invalid/browser')
        assert.deepEqual(options, { timeout: 30000 })
        return perform('connect', browser)
      }
    },
    /** Registers acquired contexts before success, a primary failure or an interrupt. */
    runCases(actualBrowser) {
      assert.strictEqual(actualBrowser, browser)
      for (const context of fixtureContexts) activeContexts.add(context)
      if (interrupt) {
        processControl.emit('SIGTERM')
        return new Promise(/** The interrupt must settle the harness, not this synthetic matrix. */ () => {})
      }
      return perform('cases', Promise.resolve())
    },
    /** Records a failure screenshot attempt without creating an artifact. */
    takeScreenshot({ page }, name) {
      assert.equal(name, 'failure')
      return perform(`screenshot ${page.name}`, Promise.resolve())
    },
    logger: {
      /** Retains diagnostic identity even when the diagnostic sink itself fails. */
      error(value) {
        errors.push(value)
        return perform('diagnostic log')
      },
      /** Makes the success marker and its ordering observable to assertions. */
      log(value) {
        logs.push(value)
        perform('success marker')
      }
    }
  }
  return { options, operations, errors, logs, server, processControl }
}

/** Captures promise settlement with an explicit status, preserving falsy rejection values. */
async function outcome(options) {
  return runBrowserHarness(options).then(
    /** A fulfilled harness has completed its real success path. */ () => ({ status: 'fulfilled' }),
    /** Records the actual rejection without deciding which failure should win. */ (error) => ({ status: 'rejected', error })
  )
}

/** Asserts complete listener disposal after a fixture whose removals do not fail. */
function listenersRemoved(fixture) {
  for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) assert.equal(fixture.processControl.listenerCount(signal), 0)
  assert.equal(fixture.server.stdout.listenerCount('data'), 0)
  assert.equal(fixture.server.stderr.listenerCount('data'), 0)
  assert.equal(fixture.server.listenerCount('close'), 0)
  assert.equal(fixture.server.listenerCount('error'), 0)
}

for (const primaryFails of [false, true]) {
  for (const cleanupFails of [false, true]) {
    test(`primary failure=${primaryFails}, cleanup failure=${cleanupFails}`, { timeout: 10000 }, /** Covers the full primary/cleanup outcome matrix and success-marker precedence. */ async () => {
      const primary = new Error('synthetic browser assertion')
      const cleanup = new Error('synthetic context close')
      const fixture = harnessFixture({ faults: {
        ...(primaryFails ? { cases: { error: primary, async: true } } : {}),
        ...(cleanupFails ? { 'context first': { error: cleanup, async: true } } : {})
      } })
      const result = await outcome(fixture.options)

      if (primaryFails) {
        assert.equal(result.status, 'rejected')
        assert.strictEqual(result.error, primary)
        assert.equal(result.error.message, 'synthetic browser assertion')
        if (cleanupFails) {
          const report = fixture.errors.find(/** Locates the separate cleanup diagnostic. */ (error) => error instanceof AggregateError)
          assert.deepEqual(report.errors, [cleanup])
        }
      } else if (cleanupFails) {
        assert.equal(result.status, 'rejected')
        assert.ok(result.error instanceof AggregateError)
        assert.equal(result.error.message, 'Browser/server cleanup failed')
        assert.deepEqual(result.error.errors, [cleanup])
      } else {
        assert.equal(result.status, 'fulfilled')
        assert.deepEqual(fixture.logs, ['Admin session expiry browser matrix passed'])
        assert.equal(fixture.operations.at(-1), 'success marker')
      }
      if (primaryFails || cleanupFails) assert.deepEqual(fixture.logs, [])
      for (const operation of ['context first', 'context second', 'browser disconnect', 'browser close', 'Vite SIGTERM', 'remove SIGINT', 'remove SIGTERM', 'remove SIGHUP']) {
        assert.ok(fixture.operations.includes(operation), `Missing cleanup: ${operation}`)
      }
      listenersRemoved(fixture)
    })
  }
}

for (const primary of [undefined, null, false, 0, '', NaN]) {
  test(`falsy primary ${String(primary)} survives cleanup and diagnostic failures`, { timeout: 10000 }, /** Rejecting with a falsy value must not look like primary success. */ async () => {
    const cleanup = new Error('secondary cleanup')
    const fixture = harnessFixture({ faults: {
      cases: { error: primary },
      'context first': { error: cleanup },
      'screenshot first': { error: new Error('secondary screenshot'), async: true },
      'diagnostic log': { error: new Error('secondary logging') }
    } })
    const result = await outcome(fixture.options)
    assert.equal(result.status, 'rejected')
    assert.strictEqual(result.error, primary)
    assert.deepEqual(fixture.logs, [])
    assert.ok(fixture.operations.includes('screenshot second'))
    assert.ok(fixture.operations.includes('context second'))
    assert.ok(fixture.errors.some(/** Requires separately observable cleanup identity despite failing logging. */ (error) => error instanceof AggregateError && error.errors[0] === cleanup))
    listenersRemoved(fixture)
  })
}

for (const cleanup of [undefined, null, false, 0, '']) {
  test(`falsy cleanup ${String(cleanup)} still fails a successful matrix`, { timeout: 10000 }, /** Cleanup rejection presence must not depend on its thrown value's truthiness. */ async () => {
    const fixture = harnessFixture({ faults: { 'context first': { error: cleanup, async: true } } })
    const result = await outcome(fixture.options)
    assert.equal(result.status, 'rejected')
    assert.ok(result.error instanceof AggregateError)
    assert.deepEqual(result.error.errors, [cleanup])
    assert.ok(fixture.operations.includes('context second'))
    assert.deepEqual(fixture.logs, [])
    listenersRemoved(fixture)
  })
}

for (const phase of ['startServer', 'startup event', 'launchServer', 'connect']) {
  test(`${phase} failure survives diagnostics and listener cleanup failure`, { timeout: 10000 }, /** Startup acquisition failures must retain identity and still release signal listeners. */ async () => {
    const primary = new Error(`synthetic ${phase} failure`)
    const cleanup = new Error('synthetic listener failure')
    const fixture = harnessFixture({
      startup: phase === 'startup event' ? 'error' : 'ready',
      startupError: primary,
      faults: {
        ...(phase === 'startup event' ? {} : { [phase]: { error: primary, async: phase !== 'startServer' } }),
        'remove SIGINT': { error: cleanup },
        'diagnostic log': { error: new Error('synthetic diagnostic rejection'), async: true }
      }
    })
    const result = await outcome(fixture.options)
    assert.equal(result.status, 'rejected')
    assert.strictEqual(result.error, primary)
    assert.deepEqual(fixture.logs, [])
    assert.ok(fixture.operations.includes('remove SIGTERM'))
    assert.ok(fixture.operations.includes('remove SIGHUP'))
    assert.equal(fixture.processControl.listenerCount('SIGTERM'), 0)
    assert.equal(fixture.processControl.listenerCount('SIGHUP'), 0)
    if (phase !== 'startServer') assert.ok(fixture.operations.includes('Vite SIGTERM'))
    if (phase === 'connect') assert.ok(fixture.operations.includes('browser close'))
  })
}

test('premature Vite exit remains the primary startup diagnostic', { timeout: 10000 }, /** A closed child is not readiness, even when later listener removal also fails. */ async () => {
  const fixture = harnessFixture({ startup: 'closed', faults: { 'remove SIGINT': { error: new Error('cleanup') } } })
  const result = await outcome(fixture.options)
  assert.equal(result.status, 'rejected')
  assert.match(result.error.message, /^Vite exited before readiness:/)
  assert.ok(!(result.error instanceof AggregateError))
  assert.deepEqual(fixture.logs, [])
})

test('interrupt remains primary when screenshots and cleanup reject', { timeout: 10000 }, /** The actual signal race must reject while all available resources are still attempted. */ async () => {
  const fixture = harnessFixture({ interrupt: true, faults: {
    'screenshot first': { error: new Error('screenshot rejection'), async: true },
    'context first': { error: new Error('close rejection'), async: true }
  } })
  const result = await outcome(fixture.options)
  assert.equal(result.status, 'rejected')
  assert.equal(result.error.message, 'Browser checks interrupted')
  assert.ok(fixture.operations.includes('screenshot second'))
  assert.ok(fixture.operations.includes('context second'))
  assert.deepEqual(fixture.logs, [])
  listenersRemoved(fixture)
})

for (const diagnostic of ['pages first', 'page first isClosed', 'screenshot first', 'diagnostic log']) {
  for (const asynchronous of diagnostic === 'screenshot first' || diagnostic === 'diagnostic log' ? [false, true] : [false]) {
    test(`${diagnostic}, asynchronous=${asynchronous}, cannot replace primary`, { timeout: 10000 }, /** Secondary enumeration, screenshot and logging failures must not hide the browser assertion. */ async () => {
      const primary = new Error('original assertion with useful stack')
      const secondary = new Error('secondary diagnostic failure')
      const stack = primary.stack
      const fixture = harnessFixture({ faults: {
        cases: { error: primary, async: true },
        [diagnostic]: { error: secondary, async: asynchronous }
      } })
      const result = await outcome(fixture.options)
      assert.equal(result.status, 'rejected')
      assert.strictEqual(result.error, primary)
      assert.equal(result.error.stack, stack)
      assert.ok(fixture.operations.includes('screenshot second'))
      assert.ok(fixture.operations.includes('context second'))
      assert.deepEqual(fixture.logs, [])
      const report = fixture.errors.find(/** Locates captured secondary failures without replacing the original error. */ (error) => error instanceof AggregateError)
      assert.ok(report.errors.includes(secondary))
      listenersRemoved(fixture)
    })
  }
}

test('sync and async cleanup failures do not skip later resources or listeners', { timeout: 10000 }, /** Aggregation must retain every failed close/kill while reaching subsequent disposals. */ async () => {
  const failures = ['context first', 'context second', 'browser disconnect', 'browser close', 'browser kill', 'Vite SIGTERM', 'Vite SIGKILL', 'remove SIGINT']
  const faults = Object.fromEntries(failures.map(/** Mixes asynchronous browser rejections with synchronous cleanup throws. */ (name) => [name, {
    error: new Error(name), async: name === 'context second' || name === 'browser kill'
  }]))
  const fixture = harnessFixture({ faults, exitOnTerm: false })
  const timeouts = []
  const forcedExitFailure = new Error('synthetic forced-exit timeout')
  /** Settles only synthetic exit deadlines immediately; records the actual timeout arguments. */
  fixture.options.wait = (promise, label, milliseconds) => {
    timeouts.push([label, milliseconds])
    if (label === 'Vite cleanup') return Promise.reject(new Error('synthetic graceful-exit timeout'))
    if (label === 'Vite kill') return Promise.reject(forcedExitFailure)
    return promise
  }
  const result = await outcome(fixture.options)
  assert.equal(result.status, 'rejected')
  assert.ok(result.error instanceof AggregateError)
  assert.deepEqual(result.error.errors, [
    ...failures.slice(0, 7).map(/** Retrieves original failure identities in teardown order. */ (name) => faults[name].error),
    forcedExitFailure, faults['remove SIGINT'].error
  ])
  assert.ok(fixture.operations.indexOf('browser kill') > fixture.operations.indexOf('browser close'))
  assert.ok(fixture.operations.indexOf('Vite SIGKILL') > fixture.operations.indexOf('Vite SIGTERM'))
  assert.deepEqual(fixture.operations.slice(-3), ['remove SIGINT', 'remove SIGTERM', 'remove SIGHUP'])
  assert.equal(fixture.processControl.listenerCount('SIGTERM'), 0)
  assert.equal(fixture.processControl.listenerCount('SIGHUP'), 0)
  assert.equal(fixture.server.stdout.listenerCount('data'), 0)
  assert.equal(fixture.server.stderr.listenerCount('data'), 0)
  assert.equal(fixture.server.listenerCount('close'), 0)
  assert.equal(fixture.server.listenerCount('error'), 0)
  assert.ok(timeouts.some(/** Retains the original finite Vite readiness deadline. */ ([label, milliseconds]) => label === 'Vite readiness' && milliseconds === 45000))
  assert.ok(timeouts.some(/** Retains the original whole-matrix deadline. */ ([label, milliseconds]) => label === 'browser matrix' && milliseconds === 360000))
  for (const label of ['context cleanup', 'browser process kill', 'Vite cleanup', 'Vite kill']) {
    assert.ok(timeouts.some(/** Every asynchronous teardown wait keeps the existing five-second bound. */ ([actual, milliseconds]) => actual === label && milliseconds === 5000), `Missing deadline: ${label}`)
  }
  assert.deepEqual(fixture.logs, [])
})

for (const asynchronous of [false, true]) {
  test(`browser close asynchronous=${asynchronous} fails even when forced kill succeeds`, { timeout: 10000 }, /** Successful forced termination must not erase the preceding browser close failure. */ async () => {
    const closeError = new Error('graceful browser close failed')
    const fixture = harnessFixture({ faults: { 'browser close': { error: closeError, async: asynchronous } } })
    const timeouts = []
    /** Records bound arguments while immediately settling synthetic browser operations. */
    fixture.options.wait = (promise, label, milliseconds) => {
      timeouts.push([label, milliseconds])
      return promise
    }
    const result = await outcome(fixture.options)
    assert.equal(result.status, 'rejected')
    assert.ok(result.error instanceof AggregateError)
    assert.deepEqual(result.error.errors, [closeError])
    assert.ok(fixture.operations.indexOf('browser kill') > fixture.operations.indexOf('browser close'))
    assert.ok(timeouts.some(/** The forced browser kill keeps its original deadline. */ ([label, milliseconds]) => label === 'browser process kill' && milliseconds === 5000))
    if (asynchronous) {
      assert.ok(timeouts.some(/** A rejected close promise still enters the bounded graceful-close wait. */ ([label, milliseconds]) => label === 'browser process cleanup' && milliseconds === 5000))
    }
    assert.deepEqual(fixture.logs, [])
    listenersRemoved(fixture)
  })
}

test('failed Vite SIGTERM is retained after successful SIGKILL', { timeout: 10000 }, /** Recovering the child cannot hide a real signaling error or print the success marker. */ async () => {
  const signalError = new Error('synthetic SIGTERM failure')
  const fixture = harnessFixture({ faults: { 'Vite SIGTERM': { error: signalError } } })
  /** Advances only the unfulfilled graceful-exit deadline; forced exit uses the real event promise. */
  fixture.options.wait = (promise, label) => {
    if (label === 'Vite cleanup') return Promise.reject(new Error('synthetic graceful-exit timeout'))
    return promise
  }
  const result = await outcome(fixture.options)
  assert.equal(result.status, 'rejected')
  assert.ok(result.error instanceof AggregateError)
  assert.deepEqual(result.error.errors, [signalError])
  assert.ok(fixture.operations.includes('Vite SIGKILL'))
  assert.deepEqual(fixture.logs, [])
  listenersRemoved(fixture)
})

test('successful Vite SIGKILL fallback completes before the success marker', { timeout: 10000 }, /** A recovered graceful-exit timeout keeps the existing successful forced-exit behavior. */ async () => {
  const fixture = harnessFixture({ exitOnTerm: false })
  const timeouts = []
  /** Advances the graceful-exit deadline without waiting or bypassing the forced-exit promise. */
  fixture.options.wait = (promise, label, milliseconds) => {
    timeouts.push([label, milliseconds])
    if (label === 'Vite cleanup') return Promise.reject(new Error('synthetic graceful-exit timeout'))
    return promise
  }
  const result = await outcome(fixture.options)
  assert.equal(result.status, 'fulfilled')
  assert.deepEqual(fixture.operations.slice(-5), ['Vite SIGKILL', 'remove SIGINT', 'remove SIGTERM', 'remove SIGHUP', 'success marker'])
  assert.ok(timeouts.some(/** Forced exit retains a finite wait after SIGKILL. */ ([label, milliseconds]) => label === 'Vite kill' && milliseconds === 5000))
  listenersRemoved(fixture)
})

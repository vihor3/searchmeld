import { createRouter, createWebHistory } from 'vue-router'
import { api } from '../api/client'
import { useSessionStore } from '../stores/session'

const LoginView = () => import('../views/LoginView.vue')
const DashboardView = () => import('../views/DashboardView.vue')
const ProvidersView = () => import('../views/ProvidersView.vue')
const TokensView = () => import('../views/TokensView.vue')
const PlaygroundView = () => import('../views/PlaygroundView.vue')
const LogsView = () => import('../views/LogsView.vue')
const AuditLogsView = () => import('../views/AuditLogsView.vue')
const SettingsView = () => import('../views/SettingsView.vue')

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', component: LoginView, meta: { public: true } },
    { path: '/', component: DashboardView },
    { path: '/providers', component: ProvidersView },
    { path: '/keys', redirect: '/providers' },
    { path: '/tokens', component: TokensView },
    { path: '/playground', component: PlaygroundView },
    { path: '/logs', component: LogsView },
    { path: '/audit', component: AuditLogsView },
    { path: '/usage', redirect: '/' },
    { path: '/settings', component: SettingsView }
  ]
})

/**
 * Return a same-origin, router-known non-public fullPath, preserving query/hash.
 * Fall back to /playground for malformed, external, ambiguous or public targets;
 * decoded controls and encoded path separators are rejected as well.
 */
export function loginRedirect(value: unknown): string {
  const fallback = '/playground'
  if (typeof value !== 'string' || !value.startsWith('/') || value.startsWith('//')) return fallback

  try {
    // URL parsing alone tolerates broken escapes and control characters.
    if (/[\u0000-\u001f\u007f]/.test(decodeURIComponent(value))) return fallback
    const url = new URL(value, window.location.origin)
    const target = router.resolve(value)
    if (url.origin !== window.location.origin || url.pathname !== target.path || /%2f|%5c/i.test(target.path)) return fallback
    if (!target.matched.length || target.matched.some((record) => record.meta.public)) return fallback
    return target.fullPath
  } catch {
    return fallback
  }
}

let guardAttempt = 0
let pendingRecovery: { revision: number; promise: Promise<void> } | null = null

/**
 * Recover through the login guard's Cookie probe, sharing only pending work for
 * the same revision. Ignore stale or public-route expiry responses; explicit
 * logout can also start from public routes and omits the return target.
 * Navigation rejections become current-revision retry state, not request replay.
 * Settlement releases only its own pending slot, preserving any newer recovery.
 */
export function recheckSession(requestRevision: number, explicitLogout = false): Promise<void> {
  const session = useSessionStore()
  const route = router.currentRoute.value
  if (requestRevision !== session.revision || (!explicitLogout && route.meta.public)) return Promise.resolve()
  if (pendingRecovery?.revision === requestRevision) return pendingRecovery.promise

  const target = explicitLogout
    ? { path: '/login', force: true }
    : { path: '/login', query: { redirect: loginRedirect(route.fullPath) }, force: true }
  const promise = router.replace(target).then(() => {}).catch((cause: unknown) => {
    if (requestRevision === session.revision) {
      session.error = cause instanceof Error && cause.message ? cause.message : '\u65e0\u6cd5\u786e\u8ba4\u767b\u5f55\u72b6\u6001\uff0c\u8bf7\u91cd\u8bd5'
      session.retryTarget = router.resolve(target).fullPath
    }
  }).finally(() => {
    if (pendingRecovery?.promise === promise) pendingRecovery = null
  })
  pendingRecovery = { revision: requestRevision, promise }
  return promise
}

/**
 * Gate every entry, including login, on a server probe rather than stored profile.
 * Retry once after an auth revision change; cancel superseded navigation or
 * unknown probe results, and use validated return targets for auth redirects.
 */
router.beforeEach(async (to) => {
  const attempt = ++guardAttempt
  const session = useSessionStore()
  let result = await session.check(api.me)
  if (attempt !== guardAttempt) return false
  if (result.revision !== session.revision) {
    // One fresh check is enough; a second auth transition owns its own navigation.
    result = await session.check(api.me)
    if (attempt !== guardAttempt || result.revision !== session.revision) return false
  }
  if (result.status === 'error') {
    session.retryTarget = to.fullPath
    return false
  }
  if (!to.meta.public && result.status === 'anonymous') {
    return { path: '/login', query: { redirect: loginRedirect(to.fullPath) } }
  }
  if (to.meta.public && result.status === 'authenticated') return loginRedirect(to.query.redirect)
  if (!to.matched.length) return '/playground'
})

export default router

import { createRouter, createWebHistory } from 'vue-router'
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

router.beforeEach((to) => {
  const session = useSessionStore()
  if (!to.meta.public && !session.token) {
    return { path: '/login', query: { redirect: loginRedirect(to.fullPath) } }
  }
  if (to.meta.public && session.token) return loginRedirect(to.query.redirect)
})

export default router

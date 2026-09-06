<template>
  <div v-if="$route.path === '/login' && logoutError?.revision === session.revision" class="session-error logout-error login-logout-error" role="alert">
    <span>{{ logoutError.message }}</span>
    <el-button :icon="RefreshRight" :loading="loggingOut" @click="logout">重试退出</el-button>
  </div>
  <section v-if="!$route.matched.length || ($route.path === '/login' && (session.checking || session.error))" class="session-gate" :aria-busy="session.checking">
    <h2>SearchMeld</h2>
    <p v-if="session.error" role="alert">{{ session.error }}</p>
    <p v-else role="status">正在确认登录状态...</p>
    <el-button v-if="session.error" :icon="RefreshRight" :loading="session.checking" @click="retrySession">重试</el-button>
  </section>
  <router-view v-else-if="$route.path === '/login'" />
  <div v-else class="app-shell">
    <aside class="float-nav" aria-label="主导航">
      <router-link class="float-brand" to="/playground" title="SearchMeld">
        <img class="float-mark" src="/icon-192.png" alt="" width="28" height="28" />
        <strong>SearchMeld</strong>
      </router-link>

      <el-menu router :default-active="activeMenu" class="float-menu">
        <el-menu-item index="/playground" title="搜索调试">
          <el-icon><Search /></el-icon>
          <span>搜索调试</span>
        </el-menu-item>
        <el-menu-item index="/" title="仪表盘">
          <el-icon><Odometer /></el-icon>
          <span>仪表盘</span>
        </el-menu-item>
        <el-menu-item index="/providers" title="平台管理">
          <el-icon><Grid /></el-icon>
          <span>平台管理</span>
        </el-menu-item>
        <el-menu-item index="/tokens" title="接口令牌">
          <el-icon><Key /></el-icon>
          <span>接口令牌</span>
        </el-menu-item>
        <el-menu-item index="/logs" title="请求日志">
          <el-icon><Document /></el-icon>
          <span>请求日志</span>
        </el-menu-item>
        <el-menu-item index="/audit" title="审计日志">
          <el-icon><List /></el-icon>
          <span>审计日志</span>
        </el-menu-item>
        <el-menu-item index="/settings" title="系统设置">
          <el-icon><Setting /></el-icon>
          <span>系统设置</span>
        </el-menu-item>
      </el-menu>

      <div class="float-foot">
        <div class="float-user" :title="session.profile?.username || 'admin'">
          <span class="float-avatar">AD</span>
          <span class="txt">{{ session.profile?.username || 'admin' }}</span>
        </div>
        <button class="float-logout" type="button" title="退出" :disabled="loggingOut || session.checking" @click="logout">
          <el-icon :size="18"><SwitchButton /></el-icon>
          <span class="txt">退出</span>
        </button>
      </div>
    </aside>

    <main class="page-main">
      <div v-if="session.error" class="session-error" role="alert">
        <span>{{ session.error }}</span>
        <el-button :icon="RefreshRight" :loading="session.checking" @click="retrySession">重试</el-button>
      </div>
      <div v-if="logoutError?.revision === session.revision" class="session-error logout-error" role="alert">
        <span>{{ logoutError.message }}</span>
        <el-button :icon="RefreshRight" :loading="loggingOut" @click="logout">重试退出</el-button>
      </div>
      <router-view v-slot="{ Component }">
        <keep-alive include="PlaygroundView">
          <component :is="Component" />
        </keep-alive>
      </router-view>
    </main>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  Document,
  Grid,
  Key,
  List,
  Odometer,
  RefreshRight,
  Search,
  Setting,
  SwitchButton
} from '@element-plus/icons-vue'
import { api, ApiError } from './api/client'
import { recheckSession } from './router'
import { useSessionStore } from './stores/session'

const route = useRoute()
const router = useRouter()
const session = useSessionStore()
const pendingLogout = ref<number | null>(null)
const logoutError = ref<{ revision: number; message: string } | null>(null)
/** Expose pending logout only for the current revision; older requests may still settle. */
const loggingOut = computed(() => pendingLogout.value === session.revision)

const activeMenu = computed(() => {
  if (route.path.startsWith('/keys')) return '/providers'
  return route.path
})

/**
 * Revoke the server session, treating 401 as already invalid, then probe again
 * through the login guard. Ignore superseded responses and expose other revocation
 * failures for manual retry without claiming browser-wide logout.
 */
async function logout() {
  if (loggingOut.value) return
  const revision = session.advanceRevision()
  pendingLogout.value = revision
  logoutError.value = null
  try {
    try {
      await api.logout()
    } catch (error) {
      if (!(error instanceof ApiError && error.status === 401)) throw error
    }
    if (revision !== session.revision) return
    await recheckSession(session.advanceRevision(), true)
  } catch (error) {
    if (revision === session.revision) {
      logoutError.value = { revision, message: error instanceof Error ? error.message : '退出失败，请重试' }
    }
  } finally {
    if (pendingLogout.value === revision) pendingLogout.value = null
  }
}

/** Retry guarded navigation to the failed or current route, not its writes or passwords. */
async function retrySession() {
  if (session.checking) return
  const target = router.resolve(session.retryTarget || route.fullPath)
  await router.replace({ path: target.path, query: target.query, hash: target.hash, force: true })
}
</script>

<style scoped>
.session-gate { min-height: 100vh; display: flex; flex-direction: column; align-items: center; justify-content: center; padding: 24px; text-align: center; }
.session-gate h2 { margin: 0; font-size: 22px; }
.session-gate p { max-width: 520px; overflow-wrap: anywhere; }
.session-error { display: flex; flex-wrap: wrap; align-items: center; gap: 12px; margin-bottom: 20px; color: var(--text); }
.session-error span { min-width: 0; overflow-wrap: anywhere; }
.login-logout-error { padding: 16px 24px; margin: 0; }
.float-user .txt { min-width: 0; overflow: hidden; text-overflow: ellipsis; }
</style>

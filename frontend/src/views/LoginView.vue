<template>
  <div class="login-page">
    <el-card class="login-card soft-card" shadow="never">
      <img class="login-logo" src="/icon-192.png" alt="SearchMeld" width="52" height="52" />
      <h2>SearchMeld</h2>
      <p class="muted">搜索中转控制台</p>
      <el-form label-position="top" @submit.prevent="login">
        <el-form-item label="用户名"><el-input v-model="form.username" /></el-form-item>
        <el-form-item label="密码"><el-input v-model="form.password" type="password" show-password /></el-form-item>
        <el-button type="primary" :loading="loading" class="full" @click="login">登录</el-button>
      </el-form>
    </el-card>
  </div>
</template>

<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus/es/components/message/index'
import { api } from '../api/client'
import { loginRedirect } from '../router'
import { useSessionStore } from '../stores/session'

const route = useRoute()
const router = useRouter()
const session = useSessionStore()
const loading = ref(false)
const form = reactive({ username: '', password: '' })

async function login() {
  if (loading.value) return
  loading.value = true
  let revision = session.advanceRevision()
  try {
    await api.login(form.username, form.password)
    if (revision !== session.revision) return
    revision = session.advanceRevision()
    form.password = ''
    await router.replace(loginRedirect(route.query.redirect))
  } catch (error) {
    if (revision === session.revision) ElMessage.error(error instanceof Error ? error.message : '登录失败')
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
.login-page { min-height: 100vh; display: grid; place-items: center; padding: 24px; }
.login-card { width: 390px; max-width: 100%; text-align: center; }
.login-logo {
  width: 52px; height: 52px; display: block; margin: 0 auto 12px;
  border-radius: 14px; object-fit: cover;
}
.full { width: 100%; }
h2 { margin: 0 0 4px; letter-spacing: -0.02em; }
</style>

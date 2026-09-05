<template>
  <div>
    <div class="page-hd">
      <h1>接口令牌</h1>
      <div class="page-actions">
        <el-button type="primary" :icon="Plus" circle title="新增令牌" :disabled="loading && !loaded" @click="openCreate" />
      </div>
    </div>

    <PageSkeleton v-if="loading && !loaded" type="table" :rows="5" />
    <template v-else>
    <el-alert v-if="rawToken" type="success" show-icon :closable="false" class="token-alert">
      <template #title>
        <div class="raw-token-row">
          <span>令牌已创建，可随时在列表中再次复制</span>
          <code>{{ rawToken }}</code>
          <el-button :icon="CopyDocument" circle size="small" title="复制" @click="copyText(rawToken)" />
        </div>
      </template>
    </el-alert>

    <el-card class="soft-card" shadow="never" v-loading="loading">
      <el-table class="token-table" :data="tokens" stripe table-layout="fixed">
        <el-table-column prop="name" label="名称" min-width="140" show-overflow-tooltip />
        <el-table-column label="权限" width="150">
          <template #default="scope">
            <div class="provider-tags">
              <el-tag v-for="item in scope.row.scopes" :key="item" size="small" type="info">{{ scopeLabel(item) }}</el-tag>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="令牌（点击复制）" min-width="220">
          <template #default="scope">
            <button
              type="button"
              class="token-copy-button"
              :disabled="copyingTokenId === scope.row.id"
              title="复制完整令牌"
              @click="copyToken(scope.row)"
            >
              <span class="mono">{{ displayToken(scope.row) }}</span>
              <el-icon v-if="copyingTokenId === scope.row.id" class="is-loading"><Loading /></el-icon>
              <el-icon v-else><CopyDocument /></el-icon>
            </button>
          </template>
        </el-table-column>
        <el-table-column label="渠道" min-width="170">
          <template #default="scope">
            <div class="provider-tags">
              <el-tag v-if="scope.row.allowed_providers.length === 0" size="small" type="info">全部</el-tag>
              <el-tag v-for="provider in scope.row.allowed_providers" :key="provider" size="small">{{ providerLabel(provider) }}</el-tag>
            </div>
          </template>
        </el-table-column>
        <el-table-column prop="status" label="状态" width="82" align="center">
          <template #default="scope">
            <el-tag :type="scope.row.status === 'enabled' ? 'success' : 'info'">
              {{ scope.row.status === 'enabled' ? '启用' : '停用' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="rate_limit_per_min" label="RPM" width="76" align="right" />
        <el-table-column label="额度" width="108">
          <template #default="scope">
            <div class="quota-cell">
              <span>日 {{ formatQuota(scope.row.daily_quota) }}</span>
              <small>月 {{ formatQuota(scope.row.monthly_quota) }}</small>
            </div>
          </template>
        </el-table-column>
        <el-table-column prop="usage_count" label="使用" width="76" align="right" />
        <el-table-column label="操作" width="132" align="center" fixed="right">
          <template #default="scope">
            <div class="row-actions">
              <el-button link :icon="Edit" title="编辑" @click="openEdit(scope.row)" />
              <el-button
                link
                :icon="scope.row.status === 'enabled' ? Remove : CircleCheck"
                :title="scope.row.status === 'enabled' ? '停用' : '启用'"
                @click="setStatus(scope.row, scope.row.status === 'enabled' ? 'disabled' : 'enabled')"
              />
              <el-button link type="danger" :icon="Delete" title="删除" @click="remove(scope.row.id)" />
            </div>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    </template>

    <el-dialog v-model="dialog" :title="editingToken ? '编辑接口令牌' : '新增接口令牌'" width="min(520px, calc(100% - 32px))">
      <el-form label-position="top">
        <el-form-item label="名称"><el-input v-model="form.name" /></el-form-item>
        <el-form-item label="接口权限">
          <div class="scope-options" role="group" aria-label="接口权限">
            <label class="scope-option" :class="{ 'is-selected': form.scopes.includes('search') }">
              <input v-model="form.scopes" type="checkbox" value="search" />
              <span>search</span>
            </label>
            <label class="scope-option" :class="{ 'is-selected': form.scopes.includes('extract') }">
              <input v-model="form.scopes" type="checkbox" value="extract" />
              <span>extract</span>
            </label>
          </div>
          <div class="form-hint">可同时选择两个权限。</div>
        </el-form-item>
        <el-form-item label="允许请求渠道">
          <el-select v-model="form.allowed_providers" multiple collapse-tags collapse-tags-tooltip placeholder="不选择表示全部渠道">
            <el-option v-for="item in providerOptions" :key="item.value" :label="item.label" :value="item.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="每分钟限制（0 表示不限）"><el-input-number v-model="form.rate_limit_per_min" :min="0" /></el-form-item>
        <el-form-item label="日额度（0 表示不限）"><el-input-number v-model="form.daily_quota" :min="0" /></el-form-item>
        <el-form-item label="月额度（0 表示不限）"><el-input-number v-model="form.monthly_quota" :min="0" /></el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialog = false">取消</el-button>
        <el-button type="primary" @click="saveToken">保存</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus/es/components/message/index'
import { CircleCheck, CopyDocument, Delete, Edit, Loading, Plus, Remove } from '@element-plus/icons-vue'
import PageSkeleton from '../components/PageSkeleton.vue'
import { api, ApiToken } from '../api/client'
import { providerLabel, providerOptions } from '../utils/providers'

const loading = ref(true)
const loaded = ref(false)
const tokens = ref<ApiToken[]>([])
const dialog = ref(false)
const rawToken = ref('')
const editingToken = ref<ApiToken | null>(null)
const copyingTokenId = ref<number | null>(null)
const form = reactive({
  name: '默认客户端',
  scopes: ['search'],
  allowed_providers: [] as string[],
  rate_limit_per_min: 0,
  daily_quota: 0,
  monthly_quota: 0
})

async function load() {
  loading.value = true
  try {
    tokens.value = (await api.tokens()).tokens
    loaded.value = true
  } finally {
    loading.value = false
  }
}

function displayToken(token: ApiToken) {
  return token.token ? token.token : `${token.token_prefix}...`
}

function formatQuota(value: number) {
  if (!value) return '不限'
  return new Intl.NumberFormat('en-US').format(value)
}

function scopeLabel(scope: string) {
  return scope.trim().toLowerCase()
}

function resetForm() {
  form.name = '默认客户端'
  form.scopes = ['search']
  form.allowed_providers = []
  form.rate_limit_per_min = 0
  form.daily_quota = 0
  form.monthly_quota = 0
}

function openCreate() {
  editingToken.value = null
  rawToken.value = ''
  resetForm()
  dialog.value = true
}

function openEdit(token: ApiToken) {
  editingToken.value = token
  form.name = token.name
  form.scopes = token.scopes || ['search']
  form.allowed_providers = [...(token.allowed_providers || [])]
  form.rate_limit_per_min = token.rate_limit_per_min
  form.daily_quota = token.daily_quota
  form.monthly_quota = token.monthly_quota || 0
  dialog.value = true
}

async function writeClipboard(text: string) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return
    } catch {
      // HTTP 或浏览器权限限制时回退到传统复制方式。
    }
  }
  const textarea = document.createElement('textarea')
  textarea.value = text
  textarea.setAttribute('readonly', '')
  textarea.style.position = 'fixed'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)
  textarea.select()
  const copied = document.execCommand('copy')
  textarea.remove()
  if (!copied) throw new Error('浏览器拒绝复制')
}

async function copyText(text: string) {
  if (!text) return
  try {
    await writeClipboard(text)
    ElMessage.success('已复制')
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '复制失败')
  }
}

async function copyToken(token: ApiToken) {
  copyingTokenId.value = token.id
  try {
    const secret = await api.revealToken(token.id)
    if (!secret.token) {
      ElMessage.warning('未找到可复制的令牌')
      return
    }
    await writeClipboard(secret.token)
    ElMessage.success('令牌已复制')
  } catch (err) {
    ElMessage.error(err instanceof Error ? err.message : '复制失败')
  } finally {
    copyingTokenId.value = null
  }
}

async function saveToken() {
  if (!form.scopes.length) {
    ElMessage.warning('请至少选择一个接口权限')
    return
  }
  if (editingToken.value) {
    await api.updateToken(editingToken.value.id, { ...form })
    ElMessage.success('令牌已保存')
  } else {
    const result = await api.createToken(form)
    rawToken.value = result.raw_token
    ElMessage.success('令牌已创建')
  }
  dialog.value = false
  await load()
}

async function setStatus(token: ApiToken, status: string) {
  await api.updateToken(token.id, { status })
  await load()
}

async function remove(id: number) {
  await api.deleteToken(id)
  await load()
}

onMounted(load)
</script>

<style scoped>
.token-alert { margin-bottom: 14px; }
.raw-token-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.raw-token-row code {
  font-family: var(--mono);
  word-break: break-all;
}
.token-table :deep(.el-table__cell) {
  padding: 10px 0;
}
.token-copy-button {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  max-width: 100%;
  padding: 4px 6px;
  border: 0;
  border-radius: 6px;
  background: transparent;
  color: var(--el-color-primary);
  cursor: pointer;
}
.token-copy-button:hover {
  background: var(--el-color-primary-light-9);
}
.token-copy-button:disabled {
  cursor: wait;
  opacity: 0.7;
}
.token-copy-button .mono {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.provider-tags { display: flex; align-items: center; flex-wrap: wrap; gap: 6px; }
.scope-options {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px;
  width: 100%;
}
.scope-option {
  display: flex;
  align-items: center;
  gap: 9px;
  min-height: 40px;
  padding: 0 13px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--card);
  color: var(--text);
  cursor: pointer;
  transition: border-color 0.16s ease, background-color 0.16s ease, box-shadow 0.16s ease;
}
.scope-option:hover {
  border-color: var(--primary);
}
.scope-option.is-selected {
  border-color: var(--primary);
  background: color-mix(in srgb, var(--primary) 8%, var(--card));
  box-shadow: 0 0 0 1px color-mix(in srgb, var(--primary) 18%, transparent);
}
.scope-option:focus-within {
  outline: 2px solid color-mix(in srgb, var(--primary) 35%, transparent);
  outline-offset: 2px;
}
.scope-option input {
  width: 16px;
  height: 16px;
  margin: 0;
  accent-color: var(--primary);
  cursor: pointer;
}
.scope-option span {
  font-family: var(--mono);
  font-size: 13px;
  line-height: 1;
}
.form-hint {
  width: 100%;
  margin-top: 6px;
  color: var(--faint);
  font-size: 12px;
  line-height: 1.4;
}
.quota-cell {
  font-variant-numeric: tabular-nums;
  line-height: 1.3;
}
.quota-cell small {
  display: block;
  color: var(--faint);
  font-size: 11px;
  margin-top: 2px;
}
.row-actions {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  width: 100%;
  white-space: nowrap;
}
.row-actions :deep(.el-button + .el-button) {
  margin-left: 0;
}
@media (max-width: 520px) {
  .scope-options {
    grid-template-columns: 1fr;
  }
}
</style>

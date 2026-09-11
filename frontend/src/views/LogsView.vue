<template>
  <div class="logs-page">
    <div class="page-hd">
      <div>
        <h1>请求日志</h1>
      </div>
      <div class="page-actions logs-actions">
        <el-switch v-model="autoRefresh" active-text="自动刷新" />
        <el-button :icon="Refresh" circle :loading="currentList.loading" aria-label="刷新" title="刷新" @click="load()" />
      </div>
    </div>

    <div class="logs-view-tabs">
      <el-tabs v-model="activeView">
        <el-tab-pane label="用户请求" name="user" />
        <el-tab-pane label="执行日志" name="execution" />
      </el-tabs>
    </div>

    <section v-if="activeView === 'user'" class="filters user-filters" aria-label="用户请求筛选">
      <el-input v-model="userFilterQ" data-log-filter="user-text" aria-label="搜索用户请求" clearable placeholder="搜索 Request ID / 路径 / IP / Token" :prefix-icon="Search" />
      <el-select v-model="userFilterOperation" data-log-filter="user-operation" aria-label="用户请求操作" clearable placeholder="全部操作">
        <el-option label="搜索" value="search" />
        <el-option label="抽取" value="extract" />
        <el-option label="MCP" value="mcp" />
      </el-select>
      <el-select v-model="userFilterHTTP" data-log-filter="user-http-status" aria-label="HTTP 状态" clearable placeholder="全部 HTTP 状态">
        <el-option label="2xx" value="2xx" />
        <el-option label="3xx" value="3xx" />
        <el-option label="4xx" value="4xx" />
        <el-option label="5xx" value="5xx" />
        <el-option label="未记录" value="unknown" />
      </el-select>
      <el-select v-if="!userFilterOperation || userFilterOperation === 'mcp'" v-model="userFilterMCP" data-log-filter="user-mcp" aria-label="MCP 错误" clearable placeholder="全部 MCP 结果">
        <el-option label="协议或工具错误" value="any" />
        <el-option label="协议错误" value="protocol" />
        <el-option label="工具错误" value="tool" />
      </el-select>
    </section>

    <template v-else>
      <section v-if="listStates.execution.loaded" class="kpi-row" aria-label="近窗执行统计">
        <div class="kpi-item"><span>近窗执行</span><b>{{ logs.length }}</b></div>
        <div class="kpi-item"><span>成功</span><b class="ok">{{ successCount }}</b></div>
        <div class="kpi-item"><span>失败</span><b class="bad">{{ failCount }}</b></div>
        <div class="kpi-item"><span>缓存命中</span><b>{{ cacheHitCount }}</b></div>
      </section>
      <section class="filters execution-filters" aria-label="执行日志筛选">
        <el-input v-model="filterQ" data-log-filter="execution-text" aria-label="搜索执行日志" clearable placeholder="搜索 query / request_id / 错误" :prefix-icon="Search" />
        <el-select v-model="filterOperation" aria-label="执行操作" clearable placeholder="全部操作">
          <el-option label="搜索" value="search" />
          <el-option label="抽取" value="extract" />
        </el-select>
        <el-select v-model="filterStatus" aria-label="执行状态" clearable placeholder="全部状态">
          <el-option label="成功" value="success" />
          <el-option label="失败" value="failed" />
        </el-select>
        <el-select v-model="filterMode" aria-label="执行模式" clearable placeholder="全部模式">
          <el-option label="并发" value="parallel" />
          <el-option label="转移" value="fallback" />
          <el-option label="单平台" value="single" />
        </el-select>
        <el-select v-model="filterCache" aria-label="执行缓存" clearable placeholder="缓存：全部">
          <el-option label="命中" value="hit" />
          <el-option label="未命中" value="miss" />
        </el-select>
      </section>
    </template>

    <div v-if="currentList.loaded" class="window-count" aria-live="polite">
      显示 {{ activeView === 'user' ? filteredUserLogs.length : filteredLogs.length }} / 已加载 {{ activeView === 'user' ? userLogs.length : logs.length }} 条
    </div>
    <div v-if="currentList.error" class="list-error">
      <el-alert :title="currentList.loaded ? '刷新失败' : '加载失败'" :description="currentList.error" type="error" :closable="false" show-icon />
      <el-button :icon="Refresh" :loading="currentList.loading" @click="load()">重试</el-button>
    </div>
    <PageSkeleton v-if="currentList.loading && !currentList.loaded" type="table" :rows="8" class="logs-skeleton" />

    <div v-show="activeView === 'user' && listStates.user.loaded" v-loading="listStates.user.loading" class="stream user-stream" :aria-busy="listStates.user.loading">
      <article
        v-for="row in filteredUserLogs"
        :key="row.id"
        class="log-card user-log-card"
        :class="{ fail: userRequestFailed(row), active: selection?.kind === 'user' && selection.id === row.id }"
        data-log-kind="user"
        :data-log-id="row.id"
        role="button"
        tabindex="0"
        :aria-label="`查看用户请求 ${row.request_id}`"
        @click="openUserDetail(row, $event)"
        @keydown.enter.prevent="openUserDetail(row, $event)"
        @keydown.space.prevent="openUserDetail(row, $event)"
      >
        <div class="rail" aria-hidden="true" />
        <div class="body">
          <div class="endpoint"><b>{{ row.method || '未记录' }}</b><code>{{ row.path || '未记录' }}</code></div>
          <div class="meta">
            <span>{{ formatTime(row.created_at) }}</span>
            <span>#{{ row.id }}</span>
            <span>{{ operationLabel(row.operation) }}</span>
            <span>{{ row.compat_format || '未记录' }}</span>
          </div>
          <div class="request-id"><code>{{ row.request_id || '未记录' }}</code></div>
          <div class="entry-caller"><span>{{ callerLabel(row) }}</span><code>IP {{ row.client_ip || '未记录' }}</code></div>
          <div class="tags entry-outcomes">
            <span class="tag" :class="httpStatusClass(row.http_status)">{{ httpStatusLabel(row.http_status) }}</span>
            <span class="tag" :class="{ bad: row.completion !== 'completed' }">{{ completionLabel(row.completion) }}</span>
            <template v-if="row.operation === 'mcp'">
              <span class="tag" :class="{ bad: row.mcp_error_count > 0 }">协议错误 {{ row.mcp_error_count }}</span>
              <span class="tag" :class="{ bad: row.mcp_tool_error_count > 0 }">工具错误 {{ row.mcp_tool_error_count }}</span>
            </template>
          </div>
        </div>
        <div class="side"><div class="lat">{{ formatLatency(row.latency_ms) }}</div></div>
      </article>
      <div v-if="!filteredUserLogs.length" class="empty muted">{{ userLogs.length ? '暂无匹配用户请求' : '暂无用户请求' }}</div>
    </div>

    <div v-show="activeView === 'execution' && listStates.execution.loaded" v-loading="listStates.execution.loading" class="stream execution-stream" :aria-busy="listStates.execution.loading">
      <article
        v-for="row in filteredLogs"
        :key="row.id"
        class="log-card"
        :class="{ fail: row.status !== 'success', active: selection?.kind === 'execution' && selection.id === row.id }"
        data-log-kind="execution"
        :data-log-id="row.id"
        role="button"
        tabindex="0"
        :aria-label="`查看执行日志 ${row.request_id}`"
        @click="openDetail(row, $event)"
        @keydown.enter.prevent="openDetail(row, $event)"
        @keydown.space.prevent="openDetail(row, $event)"
      >
        <div class="rail" aria-hidden="true" />
        <div class="body">
          <div class="q">{{ row.query || '-' }}</div>
          <div class="meta">
            <span>{{ formatTime(row.created_at) }}</span>
            <code>{{ shortRequestId(row.request_id) }}</code>
            <div class="tags">
              <span class="tag" :class="row.status === 'success' ? 'ok' : 'bad'">{{ row.status === 'success' ? '成功' : '失败' }}</span>
              <span class="tag">{{ operationLabel(row.operation) }}</span>
              <span class="tag">{{ modeLabel(row.mode) }}</span>
              <span class="tag">{{ row.compat_format || '-' }}</span>
              <span class="tag" :class="{ cache: row.cache_hit }">{{ row.cache_hit ? '缓存命中' : '未命中' }}</span>
            </div>
          </div>
          <div v-if="row.providers?.length" class="providers">
            <span v-for="p in row.providers" :key="p" class="pdot">{{ providerLabel(p) }}</span>
          </div>
          <div v-if="row.error_message" class="err-line">{{ row.error_message }}</div>
        </div>
        <div class="side">
          <div class="lat" :class="latencyClass(row)">{{ formatLatency(row.latency_ms) }}</div>
          <div class="cnt">{{ row.result_count }} 条结果</div>
        </div>
      </article>
      <div v-if="!filteredLogs.length" class="empty muted">{{ logs.length ? '暂无匹配日志' : '暂无执行日志' }}</div>
    </div>

    <Teleport to="body">
      <div v-if="drawerVisible" class="log-mask" @click="closeDrawer()" />
      <aside
        v-if="drawerVisible"
        ref="drawerElement"
        class="log-drawer open"
        :data-detail-kind="selection?.kind"
        :data-detail-id="selection?.id"
        role="dialog"
        aria-modal="true"
        aria-labelledby="log-detail-title"
        tabindex="-1"
        @keydown="onDrawerKeydown"
      >
        <template v-if="selection">
          <div v-if="selection.kind === 'execution' && selectedEntry" class="drawer-back">
            <el-button :icon="ArrowLeft" text @click="backToEntry">返回用户请求</el-button>
          </div>
          <div class="dhd">
            <div class="dhd-main">
              <h2 id="log-detail-title">{{ drawerTitle }}</h2>
              <p v-if="drawerRequestId">{{ drawerRequestId }}</p>
            </div>
            <el-button circle :icon="Close" aria-label="关闭详情" title="关闭详情" @click="closeDrawer()" />
          </div>

          <div v-if="detailError" class="detail-error">
            <el-alert title="详情加载失败" :description="detailError" type="error" :closable="false" show-icon />
            <el-button :icon="Refresh" :loading="detailLoading" @click="retryDetail">重试</el-button>
          </div>

          <div v-if="selection.kind === 'user' && selectedEntry" ref="entryDetailElement" class="entry-detail" :aria-busy="detailLoading">
            <dl class="entry-metadata">
              <div v-for="item in entryMetadata" :key="item.label">
                <dt>{{ item.label }}</dt>
                <dd>{{ item.value }}</dd>
              </div>
            </dl>
            <div class="entry-link" :data-execution-state="entryLinkState">
              <span v-if="entryLinkState === 'loading'" class="muted" role="status">正在加载关联记录...</span>
              <el-button v-else-if="entryLinkState === 'linked'" data-execution-link :icon="ArrowRight" @click="openLinkedExecution">查看执行日志</el-button>
              <p v-else-if="entryLinkState === 'none'" class="muted">未进入执行流程</p>
              <p v-else-if="entryLinkState === 'missing'" class="muted">执行记录暂不可用</p>
            </div>
          </div>

          <div v-else-if="detailLoading && !executionDetailLoaded" class="drawer-skel" aria-busy="true" aria-label="加载详情">
            <div class="sk-tabs">
              <span /><span /><span />
            </div>
            <div v-for="n in 4" :key="n" class="sk-call">
              <div class="sk-line w-40" />
              <div class="sk-line w-70" />
            </div>
          </div>

          <el-tabs v-else-if="executionDetailLoaded && selectedLog" v-model="detailTab" class="drawer-tabs">
            <el-tab-pane label="请求参数" name="params">
              <div class="kv-grid">
                <div v-for="item in requestParams" :key="item.label" class="kv">
                  <span>{{ item.label }}</span>
                  <b>{{ item.value }}</b>
                </div>
              </div>
              <div v-if="selectedLog.error_message" class="err-box">{{ selectedLog.error_message }}</div>
            </el-tab-pane>

            <el-tab-pane name="calls">
              <template #label>
                渠道调用
                <em v-if="providerCallRows.length" class="tab-count">{{ providerCallRows.length }}</em>
              </template>

              <div v-if="providerCallRows.length" class="call-list">
                <article
                  v-for="call in providerCallRows"
                  :key="call.key"
                  class="call-card"
                  :class="{ fail: !callSuccess(call) }"
                >
                  <div class="call-top" @click="toggleCall(call.key)">
                    <div class="call-title">
                      <strong>{{ providerLabel(call.provider_name) }}</strong>
                      <small>
                        {{ call.key_alias || '—' }}
                        · 第 {{ call.attempt_index || 1 }} 次
                        · {{ formatLatency(call.latency_ms) }}
                        · {{ call.result_count || 0 }} 条
                        <template v-if="call.cached"> · 缓存</template>
                        <template v-if="call.will_retry"> · 将重试</template>
                      </small>
                    </div>
                    <div class="call-actions">
                      <el-tag size="small" :type="callSuccess(call) ? 'success' : 'danger'">
                        {{ callSuccess(call) ? '成功' : '失败' }}
                      </el-tag>
                      <el-button
                        link
                        :icon="isCallOpen(call.key) ? ArrowUp : ArrowDown"
                        :title="isCallOpen(call.key) ? '收起结果' : '展开结果'"
                        :aria-label="`${isCallOpen(call.key) ? '收起' : '展开'} ${providerLabel(call.provider_name)} 第 ${call.attempt_index || 1} 次调用结果`"
                        :aria-expanded="isCallOpen(call.key)"
                      />
                    </div>
                  </div>

                  <div v-if="call.error_message" class="call-err">{{ call.error_message }}</div>

                  <div v-if="isCallOpen(call.key)" class="call-results">
                    <div v-if="call.results.length" class="result-list">
                      <div
                        v-for="(item, index) in call.results"
                        :key="resultKey(call.key, index, item)"
                        class="result-card"
                      >
                        <div class="result-row">
                          <a
                            v-if="item.url"
                            class="result-title-link"
                            :href="item.url"
                            target="_blank"
                            rel="noreferrer"
                            @click.stop
                          >{{ index + 1 }}. {{ item.title || item.url }}</a>
                          <span v-else class="result-title-link result-title-text">{{ index + 1 }}. {{ item.title || '无标题' }}</span>
                          <div class="result-row-meta">
                            <span v-if="item.score !== undefined" class="result-score">评分 {{ formatScore(item.score) }}</span>
                            <el-button
                              v-if="hasResultDetails(item)"
                              link
                              class="result-expand-button"
                              :icon="isResultOpen(resultKey(call.key, index, item)) ? ArrowUp : ArrowDown"
                              :aria-label="isResultOpen(resultKey(call.key, index, item)) ? '收起正文' : '展开正文'"
                              :title="isResultOpen(resultKey(call.key, index, item)) ? '收起正文' : '展开正文'"
                              :aria-expanded="isResultOpen(resultKey(call.key, index, item))"
                              @click.stop="toggleResultKey(resultKey(call.key, index, item))"
                            />
                          </div>
                        </div>
                        <div
                          v-if="hasResultDetails(item) && isResultOpen(resultKey(call.key, index, item))"
                          class="result-detail"
                        >
                          <p v-if="item.snippet" class="result-snippet">{{ item.snippet }}</p>
                          <p v-if="item.content" class="result-snippet result-content">{{ item.content }}</p>
                          <p v-if="item.log_content_truncated || item.raw_omitted" class="muted">日志仅保留正文或 Raw 预览，原请求响应以接口返回为准。</p>
                          <p v-if="item.content_truncated" class="muted">原始 Extract 响应因大小预算截断了正文。</p>
                        </div>
                      </div>
                    </div>
                    <div v-else class="empty-call muted">{{ callEmptyDescription(call) }}</div>
                  </div>
                </article>
              </div>
              <el-empty v-else description="暂无渠道调用记录" :image-size="72" />
            </el-tab-pane>

            <el-tab-pane name="results">
              <template #label>
                {{ selectedLog?.operation === 'extract' ? '抽取结果' : (showProviderResults ? '合并结果' : '搜索结果') }}
                <em v-if="searchResults.length" class="tab-count">{{ searchResults.length }}</em>
              </template>

              <div v-if="searchResults.length" class="result-list merged">
                <div
                  v-for="(item, index) in searchResults"
                  :key="resultKey('merged', index, item)"
                  class="result-card"
                >
                  <div class="result-row">
                    <a
                      v-if="item.url"
                      class="result-title-link"
                      :href="item.url"
                      target="_blank"
                      rel="noreferrer"
                    >{{ index + 1 }}. {{ item.title || item.url }}</a>
                    <span v-else class="result-title-link result-title-text">{{ index + 1 }}. {{ item.title || '无标题' }}</span>
                    <div class="result-row-meta">
                      <span v-if="item.score !== undefined" class="result-score">评分 {{ formatScore(item.score) }}</span>
                      <el-tag size="small">{{ resultProviderLabel(item) }}</el-tag>
                      <el-button
                        v-if="hasResultDetails(item)"
                        link
                        class="result-expand-button"
                        :icon="isResultOpen(resultKey('merged', index, item)) ? ArrowUp : ArrowDown"
                        :aria-label="isResultOpen(resultKey('merged', index, item)) ? '收起正文' : '展开正文'"
                        :title="isResultOpen(resultKey('merged', index, item)) ? '收起正文' : '展开正文'"
                        :aria-expanded="isResultOpen(resultKey('merged', index, item))"
                        @click.stop="toggleResultKey(resultKey('merged', index, item))"
                      />
                    </div>
                  </div>
                  <div
                    v-if="hasResultDetails(item) && isResultOpen(resultKey('merged', index, item))"
                    class="result-detail"
                  >
                    <p v-if="item.snippet" class="result-snippet">{{ item.snippet }}</p>
                    <p v-if="item.content" class="result-snippet result-content">{{ item.content }}</p>
                    <p v-if="item.log_content_truncated || item.raw_omitted" class="muted">日志仅保留正文或 Raw 预览，原请求响应以接口返回为准。</p>
                    <p v-if="item.content_truncated" class="muted">原始 Extract 响应因大小预算截断了正文。</p>
                  </div>
                </div>
              </div>
              <el-empty v-else :description="selectedLog?.operation === 'extract' ? '暂无抽取结果' : '暂无搜索结果'" :image-size="72" />
            </el-tab-pane>
          </el-tabs>
          <dl v-else-if="selectedLog" class="entry-metadata execution-summary">
            <div v-for="item in executionSummary" :key="item.label"><dt>{{ item.label }}</dt><dd>{{ item.value }}</dd></div>
          </dl>
        </template>
      </aside>
    </Teleport>
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { ArrowDown, ArrowLeft, ArrowRight, ArrowUp, Close, Refresh, Search } from '@element-plus/icons-vue'
import PageSkeleton from '../components/PageSkeleton.vue'
import { api, ApiError } from '../api/client'
import type { ProviderCallLog, SearchLog, UserRequestLog, UserRequestLogDetail } from '../api/client'
import { useSessionStore } from '../stores/session'
import { providerLabel } from '../utils/providers'

type SearchResultItem = {
  title?: string
  url?: string
  snippet?: string
  content?: string
  provider?: string
  providers?: string[]
  score?: number
  published_at?: string
  content_truncated?: boolean
  log_content_truncated?: boolean
  raw_omitted?: boolean
}

type ProviderResultGroup = {
  provider: string
  key_alias?: string
  status?: string
  error_type?: string
  error?: string
  latency_ms?: number
  result_count?: number
  cached?: boolean
  results?: SearchResultItem[]
}

type ProviderResultGroupView = {
  key: string
  provider: string
  key_alias?: string
  status: string
  error_type?: string
  error?: string
  latency_ms: number
  result_count: number
  cached: boolean
  results: SearchResultItem[]
}

type ProviderCallRow = ProviderCallLog & {
  key: string
  results: SearchResultItem[]
}

type SearchResponseLog = {
  results?: SearchResultItem[]
  provider_results?: ProviderResultGroup[]
  provider_calls?: ProviderCallLog[]
  meta?: Record<string, unknown>
}

type LogKind = 'user' | 'execution'
type LogSelection = { kind: LogKind; id: number }
type ListState = { loading: boolean; loaded: boolean; error: string; sequence: number }

const session = useSessionStore()
const activeView = ref<LogKind>('user')
const logs = ref<SearchLog[]>([])
const userLogs = ref<UserRequestLog[]>([])
const listStates = reactive<Record<LogKind, ListState>>({
  user: { loading: false, loaded: false, error: '', sequence: 0 },
  execution: { loading: false, loaded: false, error: '', sequence: 0 }
})
const currentList = computed(() => listStates[activeView.value])
const autoRefresh = ref(true)
let refreshTimer: ReturnType<typeof window.setInterval> | undefined
const pendingListRequests = new Set<symbol>()
let viewGeneration = 0
let disposed = false
const selection = ref<LogSelection | null>(null)
const drawerVisible = computed(() => selection.value !== null)
const drawerElement = ref<HTMLElement | null>(null)
const entryDetailElement = ref<HTMLElement | null>(null)
let drawerOpener: HTMLElement | null = null
let entryScrollTop = 0
const detailLoading = ref(false)
const detailError = ref('')
const selectedEntry = ref<UserRequestLog | null>(null)
const entryDetail = ref<UserRequestLogDetail | null>(null)
const executionDetailLoaded = ref(false)
const detailTab = ref('params')
const selectedLog = ref<SearchLog | null>(null)
const detailCalls = ref<ProviderCallLog[]>([])
const openResultKeys = ref<string[]>([])
const openCallKeys = ref<string[]>([])
let detailRequestSeq = 0

const userFilterQ = ref('')
const userFilterOperation = ref<'' | UserRequestLog['operation']>('')
const userFilterHTTP = ref<'' | '2xx' | '3xx' | '4xx' | '5xx' | 'unknown'>('')
const userFilterMCP = ref<'' | 'any' | 'protocol' | 'tool'>('')
const filterQ = ref('')
const filterOperation = ref('')
const filterStatus = ref('')
const filterMode = ref('')
const filterCache = ref('')

const successCount = computed(() => logs.value.filter((item) => item.status === 'success').length)
const failCount = computed(() => logs.value.filter((item) => item.status !== 'success').length)
const cacheHitCount = computed(() => logs.value.filter((item) => item.cache_hit).length)

/** Apply entry-only filters to the loaded window without interpreting HTTP 200 as MCP success. */
const filteredUserLogs = computed(() => {
  const q = userFilterQ.value.trim().toLowerCase()
  return userLogs.value.filter((row) => {
    if (userFilterOperation.value && row.operation !== userFilterOperation.value) return false
    if (userFilterHTTP.value) {
      const statusClass = row.http_status == null ? 'unknown' : `${Math.floor(row.http_status / 100)}xx`
      if (statusClass !== userFilterHTTP.value) return false
    }
    if (userFilterMCP.value) {
      if (row.operation !== 'mcp') return false
      if (userFilterMCP.value === 'protocol' && row.mcp_error_count <= 0) return false
      if (userFilterMCP.value === 'tool' && row.mcp_tool_error_count <= 0) return false
      if (userFilterMCP.value === 'any' && row.mcp_error_count <= 0 && row.mcp_tool_error_count <= 0) return false
    }
    return !q || [row.request_id, row.path, row.method, row.client_ip, row.token_name, row.api_token_id?.toString()]
      .some((value) => value?.toLowerCase().includes(q))
  })
})

const drawerTitle = computed(() => selection.value?.kind === 'user'
  ? `${selectedEntry.value?.method || ''} ${selectedEntry.value?.path || '用户请求'}`.trim()
  : selectedLog.value?.query || `执行日志 #${selection.value?.id ?? ''}`)
const drawerRequestId = computed(() => {
  const row = selection.value?.kind === 'user' ? selectedEntry.value : selectedLog.value
  return row ? `${row.request_id} · ${formatTime(row.created_at)}` : ''
})

/** Only a successfully resolved detail may classify absence or offer an execution link. */
const entryLinkState = computed(() => {
  if (detailLoading.value) return 'loading'
  if (detailError.value || !entryDetail.value) return 'error'
  if (entryDetail.value.log.execution_request_id == null) return 'none'
  return entryDetail.value.execution_log_id == null ? 'missing' : 'linked'
})

/** Display the request-time snapshot, keeping nullable status and identity explicit. */
const entryMetadata = computed(() => {
  const row = selectedEntry.value
  if (!row) return []
  const fields = [
    { label: '记录 ID', value: String(row.id) },
    { label: 'Request ID', value: row.request_id || '未记录' },
    { label: '请求时间', value: formatTime(row.created_at) },
    { label: '操作', value: operationLabel(row.operation) },
    { label: '格式', value: row.compat_format || '未记录' },
    { label: '方法', value: row.method || '未记录' },
    { label: '路径', value: row.path || '未记录' },
    { label: '来源 IP', value: row.client_ip || '未记录' },
    { label: '调用身份', value: callerLabel(row) },
    { label: 'Token ID', value: row.api_token_id == null ? '未记录' : String(row.api_token_id) },
    { label: 'Token 名称', value: row.token_name || '未记录' },
    { label: 'HTTP 状态', value: httpStatusLabel(row.http_status) },
    { label: '完成状态', value: completionLabel(row.completion) },
    { label: '请求耗时', value: formatLatency(row.latency_ms) },
    { label: '执行 Request ID', value: row.execution_request_id ?? '未选择' }
  ]
  if (row.operation === 'mcp') fields.push(
    { label: 'MCP 协议错误', value: String(row.mcp_error_count) },
    { label: 'MCP 工具错误', value: String(row.mcp_tool_error_count) }
  )
  return fields
})

/** Retain only known execution summary fields when its full detail could not be read. */
const executionSummary = computed(() => {
  const row = selectedLog.value
  if (!row) return []
  return [
    { label: '操作', value: operationLabel(row.operation) },
    { label: '状态', value: row.status === 'success' ? '成功' : '失败' },
    { label: '结果数', value: String(row.result_count) },
    { label: '延迟', value: formatLatency(row.latency_ms) },
    { label: '缓存', value: row.cache_hit ? '缓存命中' : '未命中' },
    { label: '格式', value: row.compat_format || '-' }
  ]
})

const filteredLogs = computed(() => {
  const q = filterQ.value.trim().toLowerCase()
  return logs.value.filter((row) => {
    if (filterOperation.value && (row.operation || 'search') !== filterOperation.value) return false
    if (filterStatus.value === 'success' && row.status !== 'success') return false
    if (filterStatus.value === 'failed' && row.status === 'success') return false
    if (filterMode.value && row.mode !== filterMode.value) return false
    if (filterCache.value === 'hit' && !row.cache_hit) return false
    if (filterCache.value === 'miss' && row.cache_hit) return false
    if (!q) return true
    return (
      row.query?.toLowerCase().includes(q) ||
      row.request_id?.toLowerCase().includes(q) ||
      row.error_message?.toLowerCase().includes(q)
    )
  })
})

const responseLog = computed(() => (selectedLog.value?.response_json || {}) as SearchResponseLog)
const searchResults = computed(() => responseLog.value.results || [])
const providerResultGroups = computed<ProviderResultGroupView[]>(() => {
  const groups = Array.isArray(responseLog.value.provider_results) ? responseLog.value.provider_results : []
  return groups.map((group, index) => {
    const results = Array.isArray(group.results) ? group.results : []
    const provider = group.provider || `provider-${index + 1}`
    return {
      key: `${provider}-${index}`,
      provider,
      key_alias: group.key_alias,
      status: group.status || 'success',
      error_type: group.error_type,
      error: group.error,
      latency_ms: Number(group.latency_ms || 0),
      result_count: Number(group.result_count ?? results.length),
      cached: Boolean(group.cached),
      results
    }
  })
})
const loggedProviderCalls = computed(() => detailCalls.value.length ? detailCalls.value : responseLog.value.provider_calls || [])
const providerCallRows = computed<ProviderCallRow[]>(() => {
  const groupsByProvider = new Map(providerResultGroups.value.map((group) => [group.provider, group]))
  const usedProviders = new Set<string>()
  const rows = loggedProviderCalls.value.map((call, index) => {
    const group = groupsByProvider.get(call.provider_name)
    const status = call.status || group?.status || 'success'
    const hasResults = status === 'success'
    usedProviders.add(call.provider_name)
    return {
      ...call,
      key: providerCallKey(call, index),
      key_alias: call.key_alias || group?.key_alias || '',
      attempt_index: call.attempt_index || 1,
      will_retry: Boolean(call.will_retry),
      status,
      error_type: call.error_type || group?.error_type || '',
      error_message: call.error_message || (hasResults ? '' : group?.error || ''),
      latency_ms: call.latency_ms || group?.latency_ms || 0,
      result_count: call.result_count || (hasResults ? group?.result_count || group?.results.length || 0 : 0),
      cached: call.cached || Boolean(group?.cached),
      results: hasResults ? group?.results || [] : []
    }
  })
  for (const group of providerResultGroups.value) {
    if (usedProviders.has(group.provider)) continue
    rows.push({
      key: group.key,
      provider_key_id: 0,
      provider_name: group.provider,
      key_alias: group.key_alias || '',
      attempt_index: 1,
      will_retry: false,
      status: group.status,
      error_type: group.error_type || '',
      error_message: group.error || '',
      latency_ms: group.latency_ms,
      result_count: group.result_count,
      cached: group.cached,
      results: group.status === 'success' ? group.results : []
    })
  }
  return rows
})
const showProviderResults = computed(() => selectedLog.value?.mode === 'parallel' && providerCallRows.value.length > 1 && providerCallRows.value.some((row) => row.results.length > 0))
const requestParams = computed(() => {
  const request = (selectedLog.value?.request_json || {}) as Record<string, unknown>
  const operation = selectedLog.value?.operation || 'search'
  const target = operation === 'extract' && Array.isArray(request.urls)
    ? request.urls.map(String).join('、')
    : String(request.query || selectedLog.value?.query || '-')
  return [
    { label: '操作', value: operationLabel(operation) },
    { label: operation === 'extract' ? '目标 URL' : '搜索词', value: target },
    { label: '模式', value: modeLabel(String(request.mode || selectedLog.value?.mode || '-')) },
    { label: '渠道', value: Array.isArray(request.providers) ? request.providers.map((item) => providerLabel(String(item))).join('、') : (selectedLog.value?.providers || []).map(providerLabel).join('、') || '-' },
    { label: '结果数', value: String(request.limit || selectedLog.value?.result_count || '-') },
    { label: '缓存策略', value: String(request.cache || selectedLog.value?.cache_policy || '-') },
    { label: '去重', value: request.dedupe === false ? '否' : '是' },
    { label: '状态', value: selectedLog.value?.status === 'success' ? '成功' : '失败' },
    { label: '延迟', value: formatLatency(selectedLog.value?.latency_ms || 0) },
    { label: '格式', value: selectedLog.value?.compat_format || '-' },
    { label: 'Request ID', value: selectedLog.value?.request_id || '-' }
  ]
})

function modeLabel(mode: string) {
  return ({ parallel: '并发', fallback: '转移', single: '单平台' } as Record<string, string>)[mode] || mode
}

/** Recognize MCP entry records while retaining the historical missing-operation Search default. */
function operationLabel(operation = 'search') {
  return ({ search: '搜索', extract: '抽取', mcp: 'MCP' } as Record<string, string>)[operation || 'search'] || operation
}

/** Show verified caller categories and their saved Token snapshot, never a guessed identity. */
function callerLabel(row: UserRequestLog) {
  if (row.auth_type === 'api_token') {
    const id = row.api_token_id == null ? 'Token（ID 未记录）' : `Token #${row.api_token_id}`
    return row.token_name ? `${id} · ${row.token_name}` : id
  }
  return ({ unknown: '未验证', anonymous: '匿名', admin_key: '管理员 Key' } as Record<string, string>)[row.auth_type] || '未知'
}

/** Preserve absent committed status instead of supplying an implicit HTTP 200. */
function httpStatusLabel(status: number | null) {
  return status == null ? 'HTTP 未记录' : `HTTP ${status}`
}

/** Color only the HTTP observation; MCP error counts remain separate badges. */
function httpStatusClass(status: number | null) {
  if (status == null) return ''
  if (status >= 200 && status < 300) return 'ok'
  return status >= 400 ? 'bad' : ''
}

/** Keep interrupted, canceled and write-error completion distinct from HTTP status. */
function completionLabel(completion: UserRequestLog['completion']) {
  return ({ completed: '已完成', interrupted: '已中断', canceled: '已取消', write_error: '响应写入失败' })[completion] || '未知'
}

/** Mark abnormal rows without changing their individual transport or MCP observations. */
function userRequestFailed(row: UserRequestLog) {
  return (row.http_status != null && row.http_status >= 400) || row.completion !== 'completed' ||
    (row.operation === 'mcp' && (row.mcp_error_count > 0 || row.mcp_tool_error_count > 0))
}

function resultProviderLabel(item: SearchResultItem, fallback = '未知渠道') {
  if (item.providers?.length) return item.providers.map(providerLabel).join(', ')
  return providerLabel(item.provider || fallback)
}

function formatScore(value: number) {
  if (!Number.isFinite(value)) return '-'
  return Number.isInteger(value) ? String(value) : value.toFixed(2)
}

function formatTime(value: string) {
  if (!value) return '-'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString('zh-CN', { hour12: false })
}

function shortRequestId(id: string) {
  if (!id) return '-'
  return id.length > 14 ? `${id.slice(0, 8)}…${id.slice(-6)}` : id
}

/** Preserve zero milliseconds and distinguish absent or invalid timing from a measured zero. */
function formatLatency(value: number | null | undefined) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '未记录'
  if (value >= 1000) return `${(value / 1000).toFixed(2)}s`
  return `${value}ms`
}

function latencyClass(row: SearchLog) {
  if (row.status !== 'success') return 'bad'
  if (Number(row.latency_ms || 0) > 2000) return 'slow'
  return ''
}

function callSuccess(call: ProviderCallLog) {
  return call.status === 'success'
}

function providerCallKey(call: ProviderCallLog, index: number) {
  return `${call.provider_name}-${call.provider_key_id || 'no-key'}-${call.attempt_index || 1}-${index}`
}

function callEmptyDescription(call: ProviderCallRow) {
  if (call.error_message) return call.will_retry ? `${call.error_message}，将换 key 重试` : call.error_message
  if (call.will_retry) return '本次失败，已换 key 重试'
  if (Number(call.result_count || 0) > 0) return `调用摘要显示 ${call.result_count} 条结果，但正文未写入日志（常见于 seed/旧数据）`
  return selectedLog.value?.operation === 'extract' ? '该渠道暂无抽取结果' : '该渠道暂无搜索结果'
}

function hasResultDetails(item: SearchResultItem) {
  return Boolean(item.snippet || item.content)
}

function resultKey(prefix: string, index: number, item: SearchResultItem) {
  return `${prefix}-${index}-${item.url || item.title || 'result'}`
}

function isResultOpen(key: string) {
  return openResultKeys.value.includes(key)
}

function toggleResultKey(key: string) {
  if (isResultOpen(key)) {
    openResultKeys.value = openResultKeys.value.filter((item) => item !== key)
    return
  }
  openResultKeys.value = [...openResultKeys.value, key]
}

function isCallOpen(key: string) {
  return openCallKeys.value.includes(key)
}

function toggleCall(key: string) {
  if (isCallOpen(key)) {
    openCallKeys.value = openCallKeys.value.filter((item) => item !== key)
    return
  }
  openCallKeys.value = [...openCallKeys.value, key]
}

/** Generations distinguish tab-away/back from the original view; revisions reject older auth work. */
function isCurrentView(generation: number, revision: number) {
  return !disposed && generation === viewGeneration && revision === session.revision
}

/** Narrow caught values without treating an unavailable response as empty data. */
function readError(cause: unknown) {
  return cause instanceof Error && cause.message ? cause.message : '请求失败，请重试'
}

/** Load one recent window; only its current success, failure and finalizer may update that view. */
async function load(kind: LogKind = activeView.value) {
  if (disposed || drawerVisible.value || kind !== activeView.value) return
  const state = listStates[kind]
  const sequence = ++state.sequence
  const pending = Symbol()
  pendingListRequests.add(pending)
  const generation = viewGeneration
  const revision = session.revision
  const isCurrent = () => isCurrentView(generation, revision) && activeView.value === kind && state.sequence === sequence
  state.loading = true
  state.error = ''
  try {
    if (kind === 'user') {
      const result = await api.userRequestLogs()
      if (!isCurrent()) return
      if (!Array.isArray(result.logs)) throw new Error('用户请求列表响应无效')
      userLogs.value = result.logs
    } else {
      const result = await api.logs()
      if (!isCurrent()) return
      if (!Array.isArray(result.logs)) throw new Error('执行日志列表响应无效')
      logs.value = result.logs
    }
    state.loaded = true
  } catch (cause) {
    if (isCurrent()) state.error = readError(cause)
  } finally {
    pendingListRequests.delete(pending)
    if (isCurrent()) state.loading = false
  }
}

/** Stop stale list callbacks without claiming their underlying network work was canceled. */
function invalidateLists() {
  for (const state of [listStates.user, listStates.execution]) {
    state.sequence += 1
    state.loading = false
  }
}

/** Keep the ten-second cadence, with no overlapping poll or polling while the drawer is open. */
function startAutoRefresh() {
  stopAutoRefresh()
  if (disposed || !autoRefresh.value || drawerVisible.value) return
  refreshTimer = window.setInterval(() => {
    if (!drawerVisible.value && pendingListRequests.size === 0 && !currentList.value.loading) void load()
  }, 10000)
}

/** Dispose the page-owned interval on disable, drawer opening and unmount. */
function stopAutoRefresh() {
  if (refreshTimer === undefined) return
  window.clearInterval(refreshTimer)
  refreshTimer = undefined
}

/** Reset provider/results state between executions without modifying the retained entry snapshot. */
function resetExecutionDetail() {
  selectedLog.value = null
  executionDetailLoaded.value = false
  detailCalls.value = []
  detailTab.value = 'calls'
  openResultKeys.value = []
  openCallKeys.value = []
}

/** Change the kind/ID selection and invalidate every earlier detail callback, including finalizers. */
function selectDetail(target: LogSelection | null) {
  detailRequestSeq += 1
  selection.value = target
  detailLoading.value = false
  detailError.value = ''
}

/** Load metadata or legacy execution detail with the same guarded success/error/finally contract. */
async function loadDetail() {
  if (disposed || !selection.value) return
  const target = { ...selection.value }
  const sequence = ++detailRequestSeq
  const generation = viewGeneration
  const revision = session.revision
  const isCurrent = () => isCurrentView(generation, revision) && sequence === detailRequestSeq &&
    selection.value?.kind === target.kind && selection.value.id === target.id
  detailLoading.value = true
  detailError.value = ''
  try {
    if (target.kind === 'user') {
      const result = await api.userRequestLogDetail(target.id)
      if (!isCurrent()) return
      if (result.log?.id !== target.id) throw new Error('用户请求详情响应无效')
      if (result.execution_log_id !== null && (!Number.isInteger(result.execution_log_id) || result.execution_log_id <= 0)) {
        throw new Error('执行记录关联响应无效')
      }
      selectedEntry.value = result.log
      entryDetail.value = result
    } else {
      const result = await api.logDetail(target.id)
      if (!isCurrent()) return
      if (result.log?.id !== target.id) throw new Error('执行日志详情响应无效')
      selectedLog.value = result.log
      detailCalls.value = Array.isArray(result.calls) ? result.calls : []
      executionDetailLoaded.value = true
    }
  } catch (cause) {
    if (!isCurrent()) return
    if (target.kind === 'user' && cause instanceof ApiError && cause.status === 404) {
      detailError.value = '用户请求记录不存在或已清理'
    } else {
      const description = target.kind === 'user' ? '用户请求详情暂不可用' : '执行详情暂不可用'
      detailError.value = `${description}：${readError(cause)}`
    }
  } finally {
    if (isCurrent()) detailLoading.value = false
  }
}

/** Save the row action for close-focus restoration and freeze any pending list refresh. */
function rememberDrawerOpener(event?: Event) {
  const target = event?.currentTarget ?? document.activeElement
  drawerOpener = target instanceof HTMLElement ? target : null
  invalidateLists()
}

/** Move focus only after this selection renders; back navigation also restores entry scroll. */
function focusDrawer(selector?: string, restoreEntryScroll = false) {
  const sequence = detailRequestSeq
  const generation = viewGeneration
  const revision = session.revision
  void nextTick(() => {
    if (!isCurrentView(generation, revision) || sequence !== detailRequestSeq || !drawerVisible.value) return
    if (restoreEntryScroll && entryDetailElement.value) entryDetailElement.value.scrollTop = entryScrollTop
    const target = (selector ? drawerElement.value?.querySelector<HTMLElement>(selector) : null) || drawerElement.value
    target?.focus({ preventScroll: true })
  })
}

/** Keep focus inside the modal when retry removes its error button during loading. */
function retryDetail() {
  void loadDetail()
  focusDrawer()
}

/** Open entry metadata immediately from its safe summary, then resolve the exact execution link. */
function openUserDetail(row: UserRequestLog, event?: Event) {
  if (disposed) return
  rememberDrawerOpener(event)
  selectedEntry.value = row
  entryDetail.value = null
  entryScrollTop = 0
  resetExecutionDetail()
  selectDetail({ kind: 'user', id: row.id })
  void loadDetail()
  focusDrawer()
}

/** Open historical execution detail directly without inventing an entry for that record. */
function openDetail(row: SearchLog, event?: Event) {
  if (disposed) return
  rememberDrawerOpener(event)
  selectedEntry.value = null
  entryDetail.value = null
  resetExecutionDetail()
  selectedLog.value = row
  selectDetail({ kind: 'execution', id: row.id })
  void loadDetail()
  focusDrawer()
}

/** Follow only the server-resolved numeric ID, even when it is outside the execution list window. */
function openLinkedExecution() {
  const id = entryDetail.value?.execution_log_id
  if (disposed || selection.value?.kind !== 'user' || entryLinkState.value !== 'linked' || id == null) return
  entryScrollTop = entryDetailElement.value?.scrollTop ?? 0
  resetExecutionDetail()
  selectDetail({ kind: 'execution', id })
  void loadDetail()
  focusDrawer()
}

/** Return to the retained entry without reloading lists or accepting a late execution response. */
function backToEntry() {
  if (disposed || !selectedEntry.value) return
  selectDetail({ kind: 'user', id: selectedEntry.value.id })
  resetExecutionDetail()
  focusDrawer('[data-execution-link]', true)
}

/** Close and invalidate details; only an unchanged view may return focus to the still-mounted row. */
function closeDrawer(restoreFocus = true) {
  const opener = drawerOpener
  selectDetail(null)
  selectedEntry.value = null
  entryDetail.value = null
  resetExecutionDetail()
  drawerOpener = null
  const sequence = detailRequestSeq
  const generation = viewGeneration
  const revision = session.revision
  if (restoreFocus) void nextTick(() => {
    if (isCurrentView(generation, revision) && sequence === detailRequestSeq && !drawerVisible.value && opener?.isConnected) {
      opener.focus({ preventScroll: true })
    }
  })
}

/** Keep keyboard focus inside the single modal and allow Escape to return to its row action. */
function onDrawerKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') {
    event.preventDefault()
    event.stopPropagation()
    closeDrawer()
    return
  }
  if (event.key !== 'Tab' || !drawerElement.value) return
  const controls = Array.from(drawerElement.value.querySelectorAll<HTMLElement>(
    'a[href], button, input, select, textarea, [tabindex]'
  )).filter((element) => element.tabIndex >= 0 && !element.hasAttribute('disabled') && element.getClientRects().length > 0)
  const first = controls[0]
  const last = controls[controls.length - 1]
  if (!first || !last) {
    event.preventDefault()
    drawerElement.value.focus()
  } else if (event.shiftKey && (document.activeElement === first || document.activeElement === drawerElement.value)) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && (document.activeElement === last || document.activeElement === drawerElement.value)) {
    event.preventDefault()
    first.focus()
  }
}

/** A return to the same tab starts new work while retaining that tab's filters and loaded rows. */
watch(activeView, (kind) => {
  viewGeneration += 1
  invalidateLists()
  closeDrawer(false)
  void load(kind)
}, { flush: 'sync' })

/** Inapplicable MCP filters must not silently hide all Search/Extract rows. */
watch(userFilterOperation, (operation) => {
  if (operation && operation !== 'mcp') userFilterMCP.value = ''
})

/** Revision changes invalidate pending work without replaying the failed protected request. */
watch(() => session.revision, () => {
  viewGeneration += 1
  invalidateLists()
  closeDrawer(false)
  for (const state of [listStates.user, listStates.execution]) state.error = '登录状态已更新，请刷新日志'
}, { flush: 'sync' })

watch([autoRefresh, drawerVisible], startAutoRefresh, { flush: 'sync' })

/** Start only the default entry window and the page-owned polling timer. */
onMounted(() => {
  void load()
  startAutoRefresh()
})

/** Dispose timers and invalidate every list/detail callback before the view is removed. */
onBeforeUnmount(() => {
  disposed = true
  viewGeneration += 1
  invalidateLists()
  closeDrawer(false)
  stopAutoRefresh()
})
</script>

<style scoped>
.logs-page {
  height: calc(100dvh - 76px);
  max-height: calc(100dvh - 76px);
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  gap: 12px;
}
.page-hd {
  flex: 0 0 auto;
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 12px;
  margin-bottom: 0;
}
.page-hd h1 { margin: 0; letter-spacing: 0; }
.logs-actions { gap: 12px; flex-wrap: wrap; }
.logs-view-tabs { flex: 0 0 auto; min-width: 0; }
.logs-view-tabs :deep(.el-tabs__header) { margin: 0; }
.logs-view-tabs :deep(.el-tabs__content) { display: none; }
.logs-skeleton {
  flex: 1 1 auto;
  min-height: 0;
  overflow: hidden;
}

.kpi-row {
  flex: 0 0 auto;
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 10px;
  padding: 0 0 10px;
  border-bottom: 1px solid var(--border);
}
.kpi-item {
  display: flex;
  flex-direction: column;
  gap: 4px;
  min-width: 0;
}
.kpi-item span { color: var(--muted); font-size: 12px; }
.kpi-item b {
  font-size: 18px;
  font-weight: 800;
  letter-spacing: 0;
  font-variant-numeric: tabular-nums;
}
.kpi-item b.ok { color: #12b76a; }
.kpi-item b.bad { color: #f04438; }

.filters {
  flex: 0 0 auto;
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  min-width: 0;
}
.filters > .el-input { flex: 1 1 260px; min-width: 0; }
.filters > .el-select { flex: 0 1 150px; width: 150px; min-width: 0; }
.window-count { flex: 0 0 auto; color: var(--muted); font-size: 12px; }
.list-error {
  flex: 0 0 auto;
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}
.list-error :deep(.el-alert) { min-width: 0; overflow-wrap: anywhere; }
.list-error > .el-button { flex: 0 0 auto; }

.stream {
  flex: 1 1 auto;
  min-height: 0;
  min-width: 0;
  overflow: auto;
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding-right: 2px;
  padding-bottom: 8px;
}
.log-card {
  flex: 0 0 auto;
  display: grid;
  grid-template-columns: 6px minmax(0, 1fr) auto;
  gap: 0 14px;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: 8px;
  overflow: hidden;
  cursor: pointer;
  transition: border-color .12s ease, transform .12s ease, box-shadow .12s ease;
}
.log-card:hover {
  border-color: #c9d2dc;
  transform: translateY(-1px);
}
.log-card.active {
  border-color: #9fd4be;
  box-shadow: 0 0 0 3px rgba(11, 110, 79, .08), var(--shadow);
}
.log-card:focus-visible { outline: 2px solid var(--primary); outline-offset: -2px; }
.rail { background: #12b76a; min-height: 100%; }
.user-log-card .rail { background: #98a2b3; }
.log-card.fail .rail { background: #f04438; }
.body { padding: 14px 0 14px 2px; min-width: 0; }
.q {
  font-weight: 800;
  font-size: 15px;
  letter-spacing: 0;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.meta {
  margin-top: 6px;
  display: flex;
  flex-wrap: wrap;
  gap: 8px 12px;
  color: var(--muted);
  font-size: 12px;
  align-items: center;
}
.meta code {
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 11px;
  color: #98a2b3;
}
.endpoint {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 10px;
  align-items: baseline;
  font-size: 14px;
}
.endpoint code { overflow-wrap: anywhere; min-width: 0; }
.request-id { margin-top: 6px; font-size: 12px; color: var(--muted); overflow-wrap: anywhere; }
.entry-caller {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 14px;
  margin-top: 6px;
  font-size: 12px;
}
.entry-caller > * { min-width: 0; overflow-wrap: anywhere; }
.entry-outcomes { margin-top: 8px; }
.tags { display: flex; gap: 6px; flex-wrap: wrap; }
.tag {
  height: 22px;
  padding: 0 8px;
  border-radius: 4px;
  font-size: 11px;
  font-weight: 650;
  background: #f2f4f7;
  color: #475467;
  display: inline-flex;
  align-items: center;
}
.tag.ok { background: #ecfdf3; color: #027a48; }
.tag.bad { background: #fef3f2; color: #b42318; }
.tag.cache { background: #e8f6f0; color: #085c42; }
.providers { display: flex; gap: 4px; margin-top: 8px; flex-wrap: wrap; }
.pdot {
  height: 20px;
  padding: 0 7px;
  border-radius: 6px;
  font-size: 11px;
  font-weight: 700;
  background: #f2f4f7;
  color: #475467;
}
.err-line {
  margin-top: 8px;
  color: #b42318;
  font-size: 12px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.side {
  padding: 14px 16px 14px 0;
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  justify-content: center;
  gap: 6px;
  min-width: 0;
}
.lat {
  font-variant-numeric: tabular-nums;
  font-weight: 800;
  font-size: 16px;
}
.lat.slow { color: #f79009; }
.lat.bad { color: #f04438; }
.cnt { color: var(--muted); font-size: 12px; }
.empty {
  padding: 28px 12px;
  text-align: center;
}
.muted { color: var(--muted); }

/* drawer chrome lives in unscoped block below (Teleport -> body) */
.tab-count {
  display: inline-flex;
  min-width: 16px;
  height: 16px;
  margin-left: 4px;
  padding: 0 5px;
  border-radius: 999px;
  background: #e8f6f0;
  color: #085c42;
  font-size: 11px;
  font-style: normal;
  font-weight: 700;
  align-items: center;
  justify-content: center;
}
.kv-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 10px;
}
.kv {
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 10px 12px;
  background: #fbfcfd;
}
.kv span {
  display: block;
  color: var(--muted);
  font-size: 11px;
}
.kv b {
  display: block;
  margin-top: 4px;
  font-size: 13px;
  word-break: break-all;
  font-weight: 700;
}
.err-box {
  margin-top: 12px;
  padding: 10px 12px;
  border-radius: 8px;
  background: #fef3f2;
  color: #b42318;
  font-size: 13px;
}

.call-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.call-card {
  border: 1px solid var(--border);
  border-radius: 8px;
  background: #fff;
  overflow: hidden;
}
.call-card.fail {
  border-color: #fecdca;
  background: linear-gradient(180deg, #fff8f7, #fff);
}
.call-top {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 10px;
  padding: 12px;
  cursor: pointer;
}
.call-title strong { display: block; font-size: 14px; }
.call-title small {
  display: block;
  margin-top: 4px;
  color: var(--muted);
  font-size: 12px;
  line-height: 1.45;
}
.call-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}
.call-err {
  margin: 0 12px 10px;
  padding: 8px 10px;
  border-radius: 8px;
  background: #fef3f2;
  color: #b42318;
  font-size: 12px;
}
.call-results {
  border-top: 1px solid var(--border);
  background: rgba(47, 148, 97, .04);
  padding: 10px 12px 12px;
}
.empty-call {
  padding: 10px 4px;
  font-size: 13px;
}

.result-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.result-list.merged { padding-top: 2px; }
.result-card {
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: #fff;
}
.result-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  min-height: 28px;
}
.result-title-link {
  min-width: 0;
  overflow: hidden;
  color: var(--text);
  font-weight: 800;
  text-overflow: ellipsis;
  white-space: nowrap;
  text-decoration: none;
}
.result-title-link:hover { color: var(--primary); }
.result-title-text { display: block; }
.result-row-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-shrink: 0;
}
.result-score {
  color: var(--muted);
  font-size: 12px;
  white-space: nowrap;
}
.result-expand-button {
  width: 24px;
  height: 24px;
  min-height: 24px;
  padding: 0;
  color: var(--primary);
}
.result-detail {
  margin-top: 8px;
  padding-top: 8px;
  border-top: 1px solid var(--border);
}
.result-snippet {
  margin: 0;
  color: var(--muted);
  line-height: 1.6;
  white-space: pre-wrap;
}
.result-content {
  margin-top: 8px;
  color: var(--text);
}

@media (max-width: 900px) {
  .side { padding-right: 12px; }
  .kv-grid { grid-template-columns: 1fr; }
}
@media (max-width: 980px) {
  .logs-page { height: calc(100dvh - 116px); max-height: calc(100dvh - 116px); }
}
@media (max-width: 720px) {
  .result-row {
    align-items: flex-start;
    flex-direction: column;
  }
  .result-row-meta { flex-wrap: wrap; }
}
@media (max-width: 560px) {
  .filters > .el-input { flex-basis: 100%; }
  .filters > .el-select { flex: 1 1 calc(50% - 4px); width: calc(50% - 4px); }
  .log-card { grid-template-columns: 4px minmax(0, 1fr); gap: 0 10px; }
  .rail { grid-row: 1 / 3; }
  .body { padding: 12px 12px 8px 0; }
  .side { grid-column: 2; padding: 0 12px 12px 0; flex-direction: row; justify-content: flex-start; align-items: baseline; gap: 12px; }
  .lat { font-size: 14px; }
  .list-error { align-items: flex-start; flex-wrap: wrap; }
}
@media (prefers-reduced-motion: reduce) {
  .log-card { transition: none; }
}
</style>

<style>
/* Teleport to body — must be unscoped */
.log-mask {
  position: fixed;
  inset: 0;
  z-index: 2000;
  background: rgba(15, 20, 25, 0.28);
  backdrop-filter: blur(2px);
}
.log-drawer {
  position: fixed;
  top: 0;
  right: 0;
  bottom: 0;
  z-index: 2001;
  width: min(560px, 100vw);
  display: flex;
  flex-direction: column;
  background: #fff;
  border-left: 1px solid var(--border, #e6e8ec);
  box-shadow: -12px 0 40px rgba(16, 24, 40, 0.12);
  transform: translateX(100%);
  transition: transform 0.18s ease;
  pointer-events: none;
  padding: 16px 18px;
  box-sizing: border-box;
  min-width: 0;
  letter-spacing: 0;
}
.log-drawer.open {
  transform: none;
  pointer-events: auto;
}
.log-drawer .dhd {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  padding: 0 0 12px;
  border-bottom: 1px solid var(--border, #e6e8ec);
  margin-bottom: 4px;
  flex: 0 0 auto;
  min-width: 0;
}
.log-drawer .dhd-main { min-width: 0; max-height: 24dvh; overflow: auto; }
.log-drawer .dhd > .el-button { flex: 0 0 auto; }
.log-drawer .drawer-back { flex: 0 0 auto; padding-bottom: 8px; }
.log-drawer .drawer-back .el-button { margin-left: 0; }
.log-drawer .dhd h2 {
  margin: 0;
  font-size: 16px;
  line-height: 1.35;
  overflow-wrap: anywhere;
}
.log-drawer .dhd p {
  margin: 4px 0 0;
  color: var(--muted, #667085);
  font-size: 12px;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  word-break: break-all;
}
.log-drawer .detail-error {
  flex: 0 0 auto;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  margin: 10px 0;
  min-width: 0;
  max-height: 24dvh;
  overflow: auto;
}
.log-drawer .detail-error .el-alert { min-width: 0; overflow-wrap: anywhere; }
.log-drawer .entry-detail,
.log-drawer .execution-summary {
  flex: 1 1 auto;
  min-height: 0;
  overflow: auto;
}
.log-drawer .entry-metadata { margin: 0; font-size: 13px; }
.log-drawer .entry-metadata > div {
  display: grid;
  grid-template-columns: 116px minmax(0, 1fr);
  gap: 8px;
  padding: 10px 0;
  border-bottom: 1px solid var(--border, #e6e8ec);
}
.log-drawer .entry-metadata dt { color: var(--muted, #667085); }
.log-drawer .entry-metadata dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
.log-drawer .entry-link { padding: 16px 0; overflow-wrap: anywhere; }
.log-drawer .entry-link p { margin: 0; }
.log-drawer .drawer-skel {
  flex: 1;
  min-height: 0;
  overflow: auto;
  padding: 4px 4px 20px;
}
.log-drawer .drawer-skel .sk-tabs {
  display: flex;
  gap: 10px;
  margin-bottom: 16px;
}
.log-drawer .drawer-skel .sk-tabs span {
  width: 72px;
  height: 28px;
  border-radius: 999px;
  background: linear-gradient(90deg, #eef1f4 25%, #f7f8fa 50%, #eef1f4 75%);
  background-size: 200% 100%;
  animation: log-skel 1.2s ease infinite;
}
.log-drawer .drawer-skel .sk-call {
  padding: 14px;
  border: 1px solid var(--border, #e6e8ec);
  border-radius: 8px;
  background: #fbfcfd;
  margin-bottom: 10px;
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.log-drawer .drawer-skel .sk-line {
  height: 12px;
  border-radius: 999px;
  background: linear-gradient(90deg, #eef1f4 25%, #f7f8fa 50%, #eef1f4 75%);
  background-size: 200% 100%;
  animation: log-skel 1.2s ease infinite;
}
.log-drawer .drawer-skel .sk-line.w-40 { width: 40%; }
.log-drawer .drawer-skel .sk-line.w-70 { width: 70%; }
@keyframes log-skel {
  0% { background-position: 100% 0; }
  100% { background-position: -100% 0; }
}
.log-drawer .drawer-tabs {
  flex: 1 1 auto;
  min-height: 0;
  min-width: 0;
  display: flex;
  flex-direction: column;
}
.log-drawer .drawer-tabs .el-tabs__header {
  flex: 0 0 auto;
}
.log-drawer .drawer-tabs .el-tabs__content {
  flex: 1 1 auto;
  min-height: 0;
  min-width: 0;
  overflow: auto;
  padding-right: 2px;
}
.log-drawer .drawer-tabs .el-tab-pane {
  padding-bottom: 16px;
}
.log-drawer .tab-count {
  display: inline-flex;
  min-width: 16px;
  height: 16px;
  margin-left: 4px;
  padding: 0 5px;
  border-radius: 999px;
  background: #e8f6f0;
  color: #085c42;
  font-size: 11px;
  font-style: normal;
  font-weight: 700;
  align-items: center;
  justify-content: center;
}
.log-drawer .kv-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 10px;
}
.log-drawer .kv {
  min-width: 0;
  border: 0;
  border-bottom: 1px solid var(--border, #e6e8ec);
  border-radius: 0;
  padding: 10px 0;
  background: transparent;
}
.log-drawer .kv span {
  display: block;
  color: var(--muted, #667085);
  font-size: 11px;
}
.log-drawer .kv b {
  display: block;
  margin-top: 4px;
  font-size: 13px;
  word-break: break-all;
  font-weight: 700;
}
.log-drawer .err-box {
  margin-top: 12px;
  padding: 10px 12px;
  border-radius: 8px;
  background: #fef3f2;
  color: #b42318;
  font-size: 13px;
  overflow-wrap: anywhere;
}
.log-drawer .call-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.log-drawer .call-card {
  border: 1px solid var(--border, #e6e8ec);
  border-radius: 8px;
  background: #fff;
  overflow: hidden;
}
.log-drawer .call-card.fail {
  border-color: #fecdca;
  background: linear-gradient(180deg, #fff8f7, #fff);
}
.log-drawer .call-top {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 10px;
  padding: 12px;
  cursor: pointer;
}
.log-drawer .call-title strong { display: block; font-size: 14px; }
.log-drawer .call-title { min-width: 0; overflow-wrap: anywhere; }
.log-drawer .call-title small {
  display: block;
  margin-top: 4px;
  color: var(--muted, #667085);
  font-size: 12px;
  line-height: 1.45;
}
.log-drawer .call-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}
.log-drawer .call-err {
  margin: 0 12px 10px;
  padding: 8px 10px;
  border-radius: 8px;
  background: #fef3f2;
  color: #b42318;
  font-size: 12px;
  overflow-wrap: anywhere;
}
.log-drawer .call-results {
  border-top: 1px solid var(--border, #e6e8ec);
  background: rgba(47, 148, 97, 0.04);
  padding: 10px 12px 12px;
}
.log-drawer .empty-call {
  padding: 10px 4px;
  font-size: 13px;
  color: var(--muted, #667085);
  overflow-wrap: anywhere;
}
.log-drawer .result-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.log-drawer .result-card {
  padding: 8px 10px;
  border: 1px solid var(--border, #e6e8ec);
  border-radius: 8px;
  background: #fff;
}
.log-drawer .call-results .result-card {
  border: 0;
  border-bottom: 1px solid var(--border, #e6e8ec);
  border-radius: 0;
  background: transparent;
  padding: 8px 0;
}
.log-drawer .result-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  min-height: 28px;
}
.log-drawer .result-title-link {
  min-width: 0;
  overflow: hidden;
  color: var(--text, #0f1419);
  font-weight: 800;
  text-overflow: ellipsis;
  white-space: nowrap;
  text-decoration: none;
  max-width: 100%;
}
.log-drawer .result-title-link:hover { color: var(--primary, #0b6e4f); }
.log-drawer .result-row-meta {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  min-width: 0;
  max-width: 100%;
}
.log-drawer .result-score {
  color: var(--muted, #667085);
  font-size: 12px;
  white-space: nowrap;
}
.log-drawer .result-detail {
  margin-top: 8px;
  padding-top: 8px;
  border-top: 1px solid var(--border, #e6e8ec);
}
.log-drawer .result-snippet {
  margin: 0;
  color: var(--muted, #667085);
  line-height: 1.6;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
.log-drawer .result-content {
  margin-top: 8px;
  color: var(--text, #0f1419);
}
@media (max-width: 720px) {
  .log-drawer .kv-grid { grid-template-columns: 1fr; }
  .log-drawer .result-row {
    align-items: flex-start;
    flex-direction: column;
  }
}
@media (max-width: 560px) {
  .log-drawer { padding: 12px; }
  .log-drawer .entry-metadata > div { grid-template-columns: 100px minmax(0, 1fr); }
  .log-drawer .call-top { flex-wrap: wrap; }
  .log-drawer .call-actions { margin-left: auto; }
}
@media (prefers-reduced-motion: reduce) {
  .log-drawer { transition: none; }
  .log-drawer .drawer-skel .sk-tabs span,
  .log-drawer .drawer-skel .sk-line { animation: none; }
}
</style>

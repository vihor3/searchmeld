import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { AdminProfile } from '../api/client'

const STORAGE_KEYS = ['searchmeld-admin-token', 'one-search-admin-token']

/** Best-effort removal of current/legacy stored tokens; storage never supplies authentication. */
function removeStoredCredentials() {
  for (const name of ['sessionStorage', 'localStorage'] as const) {
    try {
      for (const key of STORAGE_KEYS) window[name].removeItem(key)
    } catch {
      // Storage may be disabled. It is never an authentication source.
    }
  }
}

interface SessionCheck {
  revision: number
  status: 'authenticated' | 'anonymous' | 'error'
}

/**
 * Hold transient per-tab probe and UI state, removing obsolete stored credentials.
 * The server validates the Cookie for access; profile presence is not authority.
 */
export const useSessionStore = defineStore('session', () => {
  removeStoredCredentials()
  const revision = ref(0)
  const profile = ref<AdminProfile | null>(null)
  const checking = ref(false)
  const error = ref('')
  const retryTarget = ref('')
  let pending: { revision: number; promise: Promise<SessionCheck> } | null = null

  /**
   * Start a new in-tab auth revision, forgetting pending checks and retry UI.
   * Existing I/O and the displayed profile remain; late completions must compare revisions.
   */
  function advanceRevision() {
    revision.value += 1
    pending = null
    checking.value = false
    error.value = ''
    retryTarget.value = ''
    removeStoredCredentials()
    return revision.value
  }

  /**
   * Share only a pending same-revision probe, never a settled authorization result.
   * Return revision-tagged status even when superseded; only current work updates
   * profile/error UI. A null profile means anonymous; reader failures become error status.
   */
  function check(readProfile: () => Promise<AdminProfile | null>): Promise<SessionCheck> {
    if (pending?.revision === revision.value) return pending.promise
    const checkRevision = revision.value
    checking.value = true
    error.value = ''
    retryTarget.value = ''
    removeStoredCredentials()
    /** Settle a revision-tagged probe without overwriting newer UI or clearing its pending slot. */
    const promise = (async (): Promise<SessionCheck> => {
      try {
        const currentProfile = await readProfile()
        if (checkRevision === revision.value) profile.value = currentProfile
        return { revision: checkRevision, status: currentProfile ? 'authenticated' : 'anonymous' }
      } catch (cause) {
        if (checkRevision === revision.value) {
          error.value = cause instanceof Error && cause.message ? cause.message : '\u65e0\u6cd5\u786e\u8ba4\u767b\u5f55\u72b6\u6001\uff0c\u8bf7\u91cd\u8bd5'
        }
        return { revision: checkRevision, status: 'error' }
      } finally {
        if (pending?.revision === checkRevision) {
          pending = null
          checking.value = false
        }
      }
    })()
    pending = { revision: checkRevision, promise }
    return promise
  }

  return { revision, profile, checking, error, retryTarget, advanceRevision, check }
})

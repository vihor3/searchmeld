import { defineStore } from 'pinia'
import { ref } from 'vue'
import type { AdminProfile } from '../api/client'

const STORAGE_KEYS = ['searchmeld-admin-token', 'one-search-admin-token']

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

export const useSessionStore = defineStore('session', () => {
  removeStoredCredentials()
  const revision = ref(0)
  const profile = ref<AdminProfile | null>(null)
  const checking = ref(false)
  const error = ref('')
  const retryTarget = ref('')
  let pending: { revision: number; promise: Promise<SessionCheck> } | null = null

  function advanceRevision() {
    revision.value += 1
    pending = null
    checking.value = false
    error.value = ''
    retryTarget.value = ''
    removeStoredCredentials()
    return revision.value
  }

  function check(readProfile: () => Promise<AdminProfile | null>): Promise<SessionCheck> {
    if (pending?.revision === revision.value) return pending.promise
    const checkRevision = revision.value
    checking.value = true
    error.value = ''
    retryTarget.value = ''
    removeStoredCredentials()
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

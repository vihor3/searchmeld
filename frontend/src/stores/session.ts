import { defineStore } from 'pinia'

const TOKEN_KEY = 'searchmeld-admin-token'
const LEGACY_TOKEN_KEY = 'one-search-admin-token'

function initialToken() {
  localStorage.removeItem(TOKEN_KEY)
  localStorage.removeItem(LEGACY_TOKEN_KEY)
  const token = sessionStorage.getItem(TOKEN_KEY) || sessionStorage.getItem(LEGACY_TOKEN_KEY) || ''
  if (token) sessionStorage.setItem(TOKEN_KEY, token)
  sessionStorage.removeItem(LEGACY_TOKEN_KEY)
  return token
}

export const useSessionStore = defineStore('session', {
  state: () => ({ token: initialToken() }),
  actions: {
    setToken(token: string) {
      this.token = token
      sessionStorage.setItem(TOKEN_KEY, token)
    },
    logout() {
      this.token = ''
      sessionStorage.removeItem(TOKEN_KEY)
      sessionStorage.removeItem(LEGACY_TOKEN_KEY)
      localStorage.removeItem(TOKEN_KEY)
      localStorage.removeItem(LEGACY_TOKEN_KEY)
    }
  }
})

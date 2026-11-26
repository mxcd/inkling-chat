import { ref, watch } from 'vue'
import { initialTheme, type Theme } from './themeChoice'

const KEY = 'inkling-chat.theme'

// theme is applied as data-theme on <html>, where dark.css and the orbs (theme="auto") read it.
export const theme = ref<Theme>(initialTheme(localStorage.getItem(KEY), matchMedia('(prefers-color-scheme: dark)').matches))

watch(theme, value => {
  document.documentElement.dataset.theme = value
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', value === 'dark' ? '#121117' : '#fbfafb')
  localStorage.setItem(KEY, value)
}, { immediate: true })

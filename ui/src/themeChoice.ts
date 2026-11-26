export type Theme = 'light' | 'dark'

// initialTheme is the reader's last choice, else what the system prefers.
export function initialTheme(stored: string | null, prefersDark: boolean): Theme {
  return stored === 'light' || stored === 'dark' ? stored : prefersDark ? 'dark' : 'light'
}

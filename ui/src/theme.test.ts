import { expect, test } from 'bun:test'

import { initialTheme } from './themeChoice'

test('a stored choice wins over the system preference, which decides otherwise', () => {
  expect(initialTheme('dark', false)).toBe('dark')
  expect(initialTheme('light', true)).toBe('light')
  expect(initialTheme(null, true)).toBe('dark')
  expect(initialTheme('nonsense', false)).toBe('light')
})

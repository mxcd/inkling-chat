import { expect, test } from 'bun:test'
import { loadThinking, saveThinking, withReasoning } from './thinking'

class MemoryStorage {
  items = new Map<string, string>()
  getItem(key: string) { return this.items.get(key) ?? null }
  setItem(key: string, value: string) { this.items.set(key, value) }
}

test('the thinking is the last choice, off without one or with an unknown one', () => {
  const storage = new MemoryStorage() as unknown as Storage
  expect(loadThinking(storage)).toBe('off')
  saveThinking('medium', storage)
  expect(loadThinking(storage)).toBe('medium')
  storage.setItem('inkling-chat.thinking', 'max')
  expect(loadThinking(storage)).toBe('off')
})

test('the thinking of a later model call starts a paragraph', () => {
  expect(withReasoning(undefined, 'Let me ', false)).toBe('Let me ')
  expect(withReasoning('Let me ', 'list.', false)).toBe('Let me list.')
  expect(withReasoning('Let me list. ', 'Found one.', true)).toBe('Let me list.\n\nFound one.')
  expect(withReasoning(undefined, 'First.', true)).toBe('First.')
})

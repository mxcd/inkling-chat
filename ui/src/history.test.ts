import { expect, test } from 'bun:test'
import { loadConversations, saveConversations, titleOf, type Conversation } from './history'

class MemoryStorage {
  items = new Map<string, string>()
  getItem(key: string) { return this.items.get(key) ?? null }
  setItem(key: string, value: string) { this.items.set(key, value) }
}

test('conversations come back newest first, and an answer cut off by a reload is stopped', () => {
  const storage = new MemoryStorage() as unknown as Storage
  const older: Conversation = { id: 'a', title: 'old', updatedAt: 1, messages: [{ id: '1', role: 'user', content: 'hi' }] }
  const newer: Conversation = {
    id: 'b', title: 'new', updatedAt: 2, messages: [
      { id: '2', role: 'assistant', content: 'half', status: 'streaming', stats: { calls: [], live: { startedAt: 0, firstTokenAt: 1, lastTokenAt: 2, pieces: 2, thinking: 0 } }, steps: [{ id: 'c', tool: 'grep', status: 'running' }] },
      { id: '3', role: 'assistant', content: '', status: 'streaming' },
    ],
  }
  saveConversations([older, newer, { id: 'empty', title: '', updatedAt: 3, messages: [] }], storage)

  const loaded = loadConversations(storage)

  expect(loaded.map(conversation => conversation.id)).toEqual(['b', 'a'])
  expect(loaded[0].messages[0]).toMatchObject({ status: 'stopped', stats: { live: null }, steps: [{ status: 'failed' }] })
  expect(loaded[0].messages[1]).toMatchObject({ status: 'error', error: 'Stopped.' })
})

test('a damaged store starts empty', () => {
  const storage = new MemoryStorage() as unknown as Storage
  storage.setItem('inkling-chat.conversations', '{nope')
  expect(loadConversations(storage)).toEqual([])
})

test('a title is the first line, cut to sixty characters', () => {
  expect(titleOf('  Hello\nmore')).toBe('Hello')
  expect(titleOf('x'.repeat(80))).toHaveLength(60)
})

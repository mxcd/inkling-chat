import { expect, test } from 'bun:test'
import { childPath, cleanDirectory, DEFAULT_PERSONA, loadDirectory, loadPersona, saveDirectory, savePersona, shortPath } from './choices'

class MemoryStorage {
  items = new Map<string, string>()
  getItem(key: string) { return this.items.get(key) ?? null }
  setItem(key: string, value: string) { this.items.set(key, value) }
}

test('a new chat starts with the last directory and persona, the workspace and the default without', () => {
  const storage = new MemoryStorage() as unknown as Storage
  expect(loadDirectory(storage)).toBe('')
  expect(loadPersona(storage)).toBe(DEFAULT_PERSONA)
  saveDirectory('/Users/me/project', storage)
  savePersona('golang', storage)
  expect(loadDirectory(storage)).toBe('/Users/me/project')
  expect(loadPersona(storage)).toBe('golang')
  savePersona('', storage)
  expect(loadPersona(storage)).toBe(DEFAULT_PERSONA)
})

test('a typed directory loses its spaces and trailing slashes', () => {
  expect(cleanDirectory('  /Users/me/project/ ')).toBe('/Users/me/project')
  expect(cleanDirectory('/')).toBe('/')
  expect(cleanDirectory('///')).toBe('/')
  expect(cleanDirectory('   ')).toBe('')
  expect(cleanDirectory('relative/')).toBe('relative')
})

test('the composer shows the last two folders of a directory', () => {
  expect(shortPath('')).toBe('workspace')
  expect(shortPath('/')).toBe('/')
  expect(shortPath('/Users/me')).toBe('/Users/me')
  expect(shortPath('/Users/me/scratch/app')).toBe('…/scratch/app')
})

test('a subfolder is its folder and its name', () => {
  expect(childPath('/Users/me', 'app')).toBe('/Users/me/app')
  expect(childPath('/', 'Users')).toBe('/Users')
})

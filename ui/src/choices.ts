// The choices a conversation keeps (history.ts) and a new chat starts with: the directory the
// tools work in and the persona, the last ones this browser sent a message with.
const DIRECTORY_KEY = 'inkling-chat.directory'
const PERSONA_KEY = 'inkling-chat.persona'

// DEFAULT_PERSONA is the server's default persona; the empty directory is its workspace.
export const DEFAULT_PERSONA = 'assistant'

export const loadDirectory = (storage: Storage = localStorage) => storage.getItem(DIRECTORY_KEY) ?? ''
export const saveDirectory = (directory: string, storage: Storage = localStorage) => storage.setItem(DIRECTORY_KEY, directory)
export const loadPersona = (storage: Storage = localStorage) => storage.getItem(PERSONA_KEY) || DEFAULT_PERSONA
export const savePersona = (persona: string, storage: Storage = localStorage) => storage.setItem(PERSONA_KEY, persona)

// cleanDirectory is a typed directory without the spaces around it and its trailing slashes.
export const cleanDirectory = (typed: string) => {
  const trimmed = typed.trim()
  return trimmed.length > 1 ? trimmed.replace(/\/+$/, '') || '/' : trimmed
}

// shortPath is a directory as the composer shows it: its last two folders, "workspace" for the
// server's own.
export function shortPath(directory: string) {
  if (!directory) return 'workspace'
  const folders = directory.split('/').filter(Boolean)
  return folders.length <= 2 ? `/${folders.join('/')}` : `…/${folders.slice(-2).join('/')}`
}

// childPath is the path of the folder name in directory.
export const childPath = (directory: string, name: string) => (directory === '/' ? `/${name}` : `${directory}/${name}`)

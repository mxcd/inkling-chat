<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { ArrowUp, Check, Folder, FolderOpen, House } from 'lucide-vue-next'
import { ChatError, listDirectories, type DirectoryListing } from '../api'
import { childPath, cleanDirectory, shortPath } from '../choices'

// The directory the tools work in: shows it shortened and opens a folder picker. A browser does
// not tell a page the absolute path of a folder it picks, so the server lists the folders below
// its roots; the path can also be typed or pasted.
const directory = defineModel<string>({ required: true })

const open = ref(false)
const listing = ref<DirectoryListing | null>(null)
const typed = ref('')
const error = ref('')
const root = ref<HTMLElement>()
const field = ref<HTMLInputElement>()

// browse shows the folder at path; one the chat may not use shows why, and the first root
async function browse(path: string) {
  try {
    listing.value = await listDirectories(path)
    typed.value = listing.value.path
    error.value = ''
  } catch (failure) {
    error.value = failure instanceof ChatError ? failure.message : 'The server did not answer.'
    if (!listing.value && path) await browse('').then(() => { error.value ||= 'The folder was not found.' })
  }
}

async function toggle() {
  open.value = !open.value
  if (!open.value) return
  listing.value = null
  error.value = ''
  await browse(directory.value)
  await nextTick()
  field.value?.focus()
}

function choose(path: string) {
  directory.value = path
  open.value = false
}

const onKeydown = (event: KeyboardEvent) => { if (event.key === 'Escape') open.value = false }
const onPointer = (event: PointerEvent) => { if (!root.value?.contains(event.target as Node)) open.value = false }
watch(open, isOpen => {
  if (isOpen) {
    document.addEventListener('keydown', onKeydown)
    document.addEventListener('pointerdown', onPointer)
    return
  }
  document.removeEventListener('keydown', onKeydown)
  document.removeEventListener('pointerdown', onPointer)
})
onBeforeUnmount(() => { open.value = false })
</script>

<template>
  <span ref="root" class="directory-wrap">
    <button
      type="button"
      :class="['toggle', 'directory', { pressed: open }]"
      :aria-expanded="open"
      :title="`The tools work in ${directory || 'the server\'s workspace folder'}. Click to pick another folder for this chat.`"
      @click="toggle"
    >
      <Folder :size="15" />
      <span class="directory-path">{{ shortPath(directory) }}</span>
    </button>
    <div v-if="open" class="picker" role="dialog" aria-label="Pick the folder the tools work in">
      <div class="picker-path">
        <button type="button" class="icon" :disabled="!listing?.parent" aria-label="Up one folder" title="Up one folder" @click="listing && browse(listing.parent)">
          <ArrowUp :size="16" />
        </button>
        <input ref="field" v-model="typed" aria-label="Folder path" spellcheck="false" autocomplete="off" placeholder="/absolute/path" @keydown.enter.prevent="browse(cleanDirectory(typed))" />
      </div>
      <p v-if="error" class="picker-error">{{ error }}</p>
      <div v-if="listing && listing.roots.length > 1" class="picker-roots">
        <button v-for="each in listing.roots" :key="each" type="button" class="picker-root" :title="each" @click="browse(each)">{{ shortPath(each) }}</button>
      </div>
      <ul v-if="listing" class="picker-list">
        <li v-for="name in listing.directories" :key="name">
          <button type="button" @click="browse(childPath(listing.path, name))"><FolderOpen :size="15" />{{ name }}</button>
        </li>
        <li v-if="!listing.directories.length" class="picker-empty">No folders in here.</li>
      </ul>
      <div class="picker-actions">
        <button type="button" class="ghost" title="The server's workspace folder" @click="choose('')"><House :size="15" />Workspace</button>
        <button type="button" class="picker-use" :disabled="!listing" @click="listing && choose(listing.path)"><Check :size="15" />Use this folder</button>
      </div>
    </div>
  </span>
</template>

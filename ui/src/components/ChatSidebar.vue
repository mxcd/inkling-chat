<script setup lang="ts">
import { SquarePen, Trash2 } from 'lucide-vue-next'
import type { Conversation } from '../history'

defineProps<{ conversations: Conversation[]; activeId: string; open: boolean }>()
const emit = defineEmits<{ select: [id: string]; remove: [id: string]; create: []; close: [] }>()

const day = (time: number) => {
  const date = new Date(time), today = new Date()
  if (date.toDateString() === today.toDateString()) return 'Today'
  today.setDate(today.getDate() - 1)
  if (date.toDateString() === today.toDateString()) return 'Yesterday'
  return date.toLocaleDateString('en-GB', { day: 'numeric', month: 'short' })
}
</script>

<template>
  <div v-if="open" class="side-scrim" @click="emit('close')" />
  <aside :class="['side', { open }]" aria-label="Conversations">
    <button class="side-new ghost" @click="emit('create')"><SquarePen :size="16" /> New chat</button>
    <p v-if="conversations.length === 0" class="side-empty">Your conversations appear here.</p>
    <ul class="side-list">
      <li v-for="(conversation, index) in conversations" :key="conversation.id">
        <small v-if="index === 0 || day(conversations[index - 1].updatedAt) !== day(conversation.updatedAt)" class="side-day">{{ day(conversation.updatedAt) }}</small>
        <div :class="['side-item', { active: conversation.id === activeId }]">
          <button class="side-title" :title="conversation.title" :aria-current="conversation.id === activeId" @click="emit('select', conversation.id)">{{ conversation.title }}</button>
          <button class="icon side-remove" :aria-label="`Delete “${conversation.title}”`" @click="emit('remove', conversation.id)"><Trash2 :size="14" /></button>
        </div>
      </li>
    </ul>
  </aside>
</template>

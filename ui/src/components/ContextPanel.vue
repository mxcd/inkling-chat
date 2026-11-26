<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { blockUnit, blocks, composition, inWindow, type Part } from '../context'
import type { ChatMessage } from '../history'
import { count } from '../metrics'

const props = defineProps<{ messages: ChatMessage[] }>()

const PARTS: { part: Part; label: string }[] = [
  { part: 'system', label: 'System' },
  { part: 'input', label: 'Input' },
  { part: 'thinking', label: 'Thinking' },
  { part: 'output', label: 'Output' },
]

const rows = computed(() => composition(props.messages))
const used = computed(() => inWindow(rows.value))
const unit = computed(() => blockUnit(used.value))
const capacity = computed(() => {
  for (const message of [...props.messages].reverse()) {
    const length = message.stats?.calls.at(-1)?.contextLength
    if (length) return length
  }
  return 0
})
const share = computed(() => (capacity.value ? Math.min(1, used.value / capacity.value) : 0))
const tokensOf = (parts: Record<Part, number>) =>
  PARTS.filter(({ part }) => parts[part] > 0).map(({ part, label }) => `${label} ${count(parts[part])}`).join(' · ')

// follow the newest turn unless the reader scrolled up
const scroller = ref<HTMLElement>()
let pinned = true
const onScroll = () => {
  const element = scroller.value!
  pinned = element.scrollHeight - element.scrollTop - element.clientHeight < 32
}
watch(used, async () => {
  if (!pinned) return
  await nextTick()
  scroller.value?.scrollTo({ top: scroller.value.scrollHeight })
})
</script>

<template>
  <aside class="ctx" aria-label="Context window">
    <header class="ctx-head">
      <h2>Context window</h2>
      <p class="ctx-total">
        {{ count(used) }}<template v-if="capacity"> / {{ count(capacity) }} <small>{{ (share * 100).toFixed(1) }} %</small></template>
      </p>
      <div v-if="capacity" class="ctx-meter" role="meter" :aria-valuenow="used" aria-valuemin="0" :aria-valuemax="capacity" aria-label="Context window used">
        <span :style="{ width: `${share * 100}%` }" />
      </div>
      <ul class="ctx-legend">
        <li v-for="{ part, label } in PARTS" :key="part"><i :class="`ctx-b ctx-${part}`" />{{ label }}</li>
      </ul>
      <p class="ctx-unit">1 block = {{ count(unit) }} tokens · ≈ estimated split</p>
    </header>
    <div ref="scroller" class="ctx-rows" @scroll.passive="onScroll">
      <p v-if="rows.length === 0" class="ctx-empty">Send a message to see what fills the context.</p>
      <section v-for="(row, index) in rows" :key="row.label.startsWith('System') ? 'system' : row.label" :class="['ctx-row', { live: row.live, past: index < rows.length - 1 }]" :title="tokensOf(row.parts)">
        <h3>{{ row.label }}<template v-if="row.estimated"> ≈</template> <small>{{ count(row.parts.system + row.parts.input + row.parts.thinking + row.parts.output) }}</small></h3>
        <div class="ctx-blocks">
          <template v-for="{ part } in PARTS" :key="part">
            <i v-for="index in blocks(row.parts[part], unit)" :key="index" :class="`ctx-b ctx-${part}`" />
          </template>
        </div>
      </section>
    </div>
  </aside>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { parseRich } from '../rich'

const props = defineProps<{ text: string; animate: boolean }>()
const blocks = computed(() => parseRich(props.text))
</script>

<template>
  <div class="rich">
    <template v-for="block in blocks" :key="block.key">
      <pre v-if="block.kind === 'code'"><code>{{ block.code }}</code></pre>
      <p v-else-if="block.kind === 'p'">
        <template v-if="animate"><span v-for="word in block.words" :key="word.key" class="w">{{ word.text }}</span></template>
        <template v-else>{{ block.words.map(word => word.text).join('') }}</template>
      </p>
      <ul v-else>
        <li v-for="(item, index) in block.items" :key="index">
          <template v-if="animate"><span v-for="word in item" :key="word.key" class="w">{{ word.text }}</span></template>
          <template v-else>{{ item.map(word => word.text).join('') }}</template>
        </li>
      </ul>
    </template>
  </div>
</template>

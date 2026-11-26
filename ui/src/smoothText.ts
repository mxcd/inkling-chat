import { onScopeDispose, ref, watch, type Ref } from 'vue'

// useSmoothText releases streamed text at a steady pace instead of in network-sized bursts:
// every frame it reveals a slice proportional to the backlog. There is no upper bound on the
// slice, so it keeps up with a model that writes hundreds of tokens per second and never shows
// the text later than about a tenth of a second after it arrived.
export function useSmoothText(target: Ref<string>, animate: Ref<boolean>) {
  const shown = ref(animate.value ? '' : target.value)
  let frame = 0
  const step = () => {
    frame = 0
    const backlog = target.value.length - shown.value.length
    if (backlog <= 0) return
    shown.value = target.value.slice(0, shown.value.length + Math.max(1, Math.ceil(backlog / 6)))
    frame = requestAnimationFrame(step)
  }
  watch([target, animate], () => {
    if (!animate.value) {
      cancelAnimationFrame(frame)
      frame = 0
      shown.value = target.value
    } else if (!frame) {
      frame = requestAnimationFrame(step)
    }
  }, { immediate: true })
  onScopeDispose(() => cancelAnimationFrame(frame))
  return shown
}

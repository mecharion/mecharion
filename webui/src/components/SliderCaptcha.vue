<script setup lang="ts">
import { ref } from 'vue'

// 滑块拼图。
//
// **它不提供安全性**（ADR-0037）：ML + 轨迹重放可破。它的作用是让「有验证」
// 这件事看得见，符合使用习惯。真正的成本压制在 PoW，挡慢速撞库的是限流。
const props = defineProps<{
  background: string
  piece: string
  pieceY: number
  disabled?: boolean
}>()
const emit = defineEmits<{ (e: 'change', x: number): void }>()

const BG_W = 320
const PIECE = 52
const maxX = BG_W - PIECE

const x = ref(0)
const dragging = ref(false)
let startPointer = 0
let startX = 0

function onDown(e: PointerEvent) {
  if (props.disabled) return
  dragging.value = true
  startPointer = e.clientX
  startX = x.value
  ;(e.target as HTMLElement).setPointerCapture(e.pointerId)
}
function onMove(e: PointerEvent) {
  if (!dragging.value) return
  const next = Math.min(maxX, Math.max(0, startX + (e.clientX - startPointer)))
  x.value = next
  emit('change', Math.round(next))
}
function onUp() {
  dragging.value = false
}

defineExpose({
  reset() {
    x.value = 0
    emit('change', 0)
  },
})
</script>

<template>
  <div class="select-none">
    <div class="relative overflow-hidden rounded" :style="{ width: BG_W + 'px' }">
      <img :src="background" alt="" class="block w-full" draggable="false" />
      <img
        :src="piece"
        alt=""
        draggable="false"
        class="absolute"
        :style="{ left: x + 'px', top: pieceY + 'px', width: PIECE + 'px' }"
      />
    </div>

    <div
      class="relative mt-3 h-9 rounded bg-gray-100"
      :style="{ width: BG_W + 'px' }"
    >
      <div class="absolute inset-0 flex items-center justify-center text-xs text-gray-400">
        拖动滑块完成拼图
      </div>
      <div
        class="absolute top-0 h-9 w-9 cursor-grab rounded bg-white shadow flex items-center justify-center active:cursor-grabbing"
        :class="{ 'opacity-50': disabled }"
        :style="{ left: x + 'px' }"
        @pointerdown="onDown"
        @pointermove="onMove"
        @pointerup="onUp"
        @pointercancel="onUp"
      >
        →
      </div>
    </div>
  </div>
</template>

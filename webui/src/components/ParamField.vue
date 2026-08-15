<script setup lang="ts">
import { computed } from 'vue'
import type { FormParam } from '../lib/api'

const props = defineProps<{
  param: FormParam
  /** 当前编辑值。undefined 表示「没动过」，与「改成了空」不是一回事 */
  modelValue: unknown
  disabled?: boolean
}>()
const emit = defineEmits<{ 'update:modelValue': [unknown] }>()

/**
 * 类型 → 控件。
 *
 * `list<T>` 拆出元素类型：一个 `list<port>` 该按端口校验每一项，
 * 当成 `list<string>` 处理会让非法端口一路溜到服务端才被发现。
 */
const elemType = computed(() => {
  const m = /^list<(.+)>$/.exec(props.param.type)
  return m ? m[1] : null
})
const isList = computed(() => elemType.value !== null)
const base = computed(() => elemType.value ?? props.param.type)

/** 数值型才给 el-input-number：它在 string 上会把 "1Gi" 变成 1 */
const numeric = computed(() => base.value === 'int' || base.value === 'float')

/**
 * 只读的字段一律禁用。
 *
 * `readOnly`（from / generate）是「你永远填不了」，`immutable` 是
 * 「你本来能填，但这个组件已经部署了」——两者都禁用，但**提示语不同**，
 * 由调用方那边的标签负责说清楚。
 */
const off = computed(() => props.disabled || props.param.readOnly || props.param.immutable)

/** 没动过时显示服务端给的当前值 */
const current = computed(() => (props.modelValue !== undefined ? props.modelValue : props.param.value))

function set(v: unknown) {
  emit('update:modelValue', v)
}

/** 列表以逗号分隔编辑：真正的多值编辑器不值得为 v1 造 */
const listText = computed(() => {
  const v = current.value
  return Array.isArray(v) ? v.join(', ') : v == null ? '' : String(v)
})

function setList(text: string) {
  const items = text
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s !== '')
  set(items.map((s) => (numeric.value ? Number(s) : s)))
}

const text = computed(() => {
  const v = current.value
  return v === null || v === undefined ? '' : String(v)
})
</script>

<template>
  <!-- 算不出来：与「空值」不是一回事，空值看起来像「没配」 -->
  <el-input v-if="param.pending" disabled placeholder="部署后确定" />

  <!--
    secret 永不回显。占位符说的是**设过没有**，不是掩码——
    掩码会让人以为改一个字符就能改口令，而实际上提交的是整个新值。
  -->
  <el-input
    v-else-if="base === 'secret'"
    type="password"
    show-password
    :disabled="off"
    :model-value="(modelValue as string) ?? ''"
    :placeholder="param.set ? '已设置（留空则不改）' : '未设置'"
    @update:model-value="set($event === '' ? undefined : $event)"
  />

  <el-switch
    v-else-if="base === 'bool' && !isList"
    :model-value="current === true"
    :disabled="off"
    @update:model-value="set($event)"
  />

  <el-select
    v-else-if="base === 'enum' && !isList"
    :model-value="current"
    :disabled="off"
    class="w-full"
    @update:model-value="set($event)"
  >
    <el-option v-for="v in param.values ?? []" :key="String(v)" :label="String(v)" :value="v as any" />
  </el-select>

  <el-input-number
    v-else-if="numeric && !isList"
    :model-value="current === null || current === undefined ? undefined : Number(current)"
    :min="param.min !== undefined ? Number(param.min) : undefined"
    :max="param.max !== undefined ? Number(param.max) : undefined"
    :step="base === 'float' ? 0.01 : 1"
    :precision="base === 'float' ? 2 : 0"
    :disabled="off"
    controls-position="right"
    class="w-full"
    @update:model-value="set($event)"
  />

  <el-input
    v-else-if="isList"
    :model-value="listText"
    :disabled="off"
    placeholder="逗号分隔"
    @update:model-value="setList($event)"
  />

  <el-input
    v-else
    :model-value="text"
    :disabled="off"
    @update:model-value="set($event)"
  >
    <template v-if="param.unit" #append>{{ param.unit }}</template>
  </el-input>
</template>

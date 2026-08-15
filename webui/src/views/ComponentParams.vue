<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import AppShell from '../components/AppShell.vue'
import ParamField from '../components/ParamField.vue'
import GroupEditor from '../components/GroupEditor.vue'
import { usePolled } from '../lib/useApi'
import { useParamEdits } from '../lib/useParamEdits'
import { api, apiQuery, ApiError, seg } from '../lib/api'
import type {
  FormView,
  FormParam,
  SetParamsResult,
  StatusView,
  ConfigGroupDetail,
} from '../lib/api'

const route = useRoute()
const router = useRouter()
const name = route.params.name as string

const role = ref((route.query.role as string) ?? '')
const group = ref((route.query.group as string) ?? '')
const showAdvanced = ref(false)

const path = computed(
  () => `/components/${seg(name)}/params` + apiQuery({ role: role.value, group: group.value }),
)

const form = usePolled<FormView>(path, 15000)
const v = computed(() => form.data.value)

// 组编辑器要知道「这个角色下有哪些实例」（成员只能从中挑）与现有的组
const status = usePolled<StatusView>(`/components/${seg(name)}/status`, 30000)
const groups = usePolled<{ groups: ConfigGroupDetail[] }>(`/components/${seg(name)}/groups`, 30000)

async function reloadGroups() {
  await Promise.all([groups.reload(), form.reload(), status.reload()])
}

// 编辑状态抽在 useParamEdits 里——那三条规则（只提交动过的、unset 与
// 空值分开、两者互斥）每一条错了都会毁掉用户输入，值得有自己的测试。
const ed = useParamEdits()
const { edits, unset, dirty, changeCount } = ed

// 轮询会刷新表单。**有未保存的改动时停下**：否则用户正在填的值会被
// 服务端那份覆盖掉，而那看起来像「输入框自己清空了」。
watch(dirty, (d) => {
  if (d) form.stop()
  else form.start()
})

watch(v, (val) => {
  if (val && !role.value) role.value = val.role
})

watch([role, group], () => {
  ed.reset() // 换坐标就是换一份表单，旧的编辑不该跟过去
  router.replace({ query: { role: role.value || undefined, group: group.value || undefined } })
})

function onRoleChange() {
  group.value = ''
}

const sections = computed(() => {
  const out: { title: string; params: FormParam[] }[] = []
  for (const p of v.value?.params ?? []) {
    if (!showAdvanced.value && p.advanced) continue
    const title = p.group || '常规'
    const last = out[out.length - 1]
    if (last && last.title === title) last.params.push(p)
    else out.push({ title, params: [p] })
  }
  return out
})

const advancedCount = computed(() => (v.value?.params ?? []).filter((p) => p.advanced).length)

const sourceLabel: Record<string, string> = {
  default: 'Pack 默认',
  component: '组件级',
  role: '角色级',
  group: '本组覆盖',
  derived: '推导得出',
  generated: '引擎生成',
  defaultFrom: '按事实算出',
}

function sourceType(s: string): 'primary' | 'success' | 'info' | 'warning' {
  if (s === 'group') return 'success'
  if (s === 'role' || s === 'component') return 'warning'
  if (s === 'derived' || s === 'generated') return 'primary'
  return 'info'
}

const currentGroup = computed(() => (v.value?.groups ?? []).find((g) => g.name === group.value))

// ── 保存 ────────────────────────────────────────────────────────────────

const preview = ref<SetParamsResult | null>(null)
const previewing = ref(false)
const saving = ref(false)
const error = ref('')

function body(dryRun: boolean) {
  return ed.body(dryRun, { role: role.value, group: group.value })
}

/**
 * 保存前先干跑一遍。
 *
 * 验收表第 11 条要的是「保存前告知会重启」，而那句话得先算出来——
 * 一个只说「确定要保存吗」的确认没有价值。这里复用服务端的 dryRun，
 * 不在前端猜：前端只知道每个参数的标志，不知道**这一次改动合起来**
 * 会动到哪几台机器。
 */
async function doPreview() {
  error.value = ''
  previewing.value = true
  try {
    preview.value = await api.patch<SetParamsResult>(`/components/${name}/params`, body(true))
  } catch (e) {
    preview.value = null
    error.value = e instanceof ApiError ? e.message : String(e)
  } finally {
    previewing.value = false
  }
}

async function doSave() {
  saving.value = true
  try {
    const out = await api.patch<SetParamsResult>(`/components/${name}/params`, body(false))
    ElMessage.success(`已保存，${out.changed?.length ?? 0} 个实例的规格发生变化`)
    ed.reset()
    preview.value = null
    form.start()
    await form.reload()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : String(e)
  } finally {
    saving.value = false
  }
}

function discard() {
  ed.reset()
  preview.value = null
  error.value = ''
  form.start()
}

const effectText: Record<string, string> = {
  restart: '会重启服务',
  reload: '会触发 reload',
  none: '不需要重启或 reload',
}

/** 只有会重启 / reload 才拦一下：每次都拦会训练用户无脑点确定 */
const needsConfirm = computed(() => preview.value !== null && preview.value.effect !== 'none')
</script>

<template>
  <AppShell>
    <div class="flex items-baseline gap-3">
      <router-link :to="`/components/${name}`" class="text-blue-600">{{ name }}</router-link>
      <h1 class="text-xl font-semibold">参数</h1>
      <span v-if="v" class="text-gray-500">{{ v.pack }} {{ v.version }}</span>
    </div>

    <el-alert
      v-if="form.error.value"
      type="error"
      :closable="false"
      class="mt-4"
      :title="form.error.value"
    />
    <el-alert
      v-for="w in v?.warnings ?? []"
      :key="w"
      type="warning"
      :closable="false"
      class="mt-3"
      :title="w"
    />

    <el-card class="mt-4" shadow="never">
      <div class="flex flex-wrap items-center gap-4">
        <div class="flex items-center gap-2">
          <span class="text-sm text-gray-500">角色</span>
          <el-select v-model="role" size="small" class="w-40" @change="onRoleChange">
            <el-option v-for="r in v?.roles ?? []" :key="r" :label="r" :value="r" />
          </el-select>
        </div>

        <div class="flex items-center gap-2">
          <span class="text-sm text-gray-500">配置组</span>
          <el-select v-model="group" size="small" class="w-48" placeholder="角色级（全部实例）">
            <el-option label="角色级（全部实例）" value="" />
            <el-option
              v-for="g in v?.groups ?? []"
              :key="g.name"
              :label="`${g.name}（${g.members.length} 台）`"
              :value="g.name"
            />
          </el-select>
        </div>

        <el-checkbox v-if="advancedCount" v-model="showAdvanced" size="small">
          显示高级参数（{{ advancedCount }}）
        </el-checkbox>
      </div>

      <div class="mt-3 text-sm text-gray-600">
        <template v-if="currentGroup">
          改动只影响本组的 {{ currentGroup.members.length }} 台：
          <span class="font-mono">{{ currentGroup.members.join(', ') }}</span>
        </template>
        <template v-else>
          <span class="text-amber-700">
            当前是<b>角色级</b>取值——在这里改会影响 {{ role || '该角色' }} 的<b>全部</b>实例。
          </span>
        </template>
      </div>
    </el-card>

    <el-alert v-if="error" type="error" :closable="false" class="mt-4" :title="error" />

    <GroupEditor
      v-if="v"
      class="mt-4"
      :component="name"
      :role="v.role"
      :status="status.data.value"
      :groups="groups.data.value?.groups ?? []"
      @changed="reloadGroups"
    />

    <el-card v-for="sec in sections" :key="sec.title" class="mt-4" shadow="never">
      <div class="mb-4 font-medium">{{ sec.title }}</div>

      <div v-for="p in sec.params" :key="p.name" class="mb-5">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-mono text-sm">{{ p.name }}</span>
          <span class="text-xs text-gray-400">{{ p.type }}</span>

          <el-tag v-if="p.required" type="danger" size="small" effect="plain">必填</el-tag>
          <el-tag :type="sourceType(p.source)" size="small">
            {{ sourceLabel[p.source] ?? p.source }}
          </el-tag>

          <el-tag v-if="p.immutable" type="warning" size="small" effect="plain">
            已部署，改它需重建组件
          </el-tag>
          <el-tag v-else-if="p.readOnly" type="info" size="small" effect="plain">只读</el-tag>

          <el-tag v-if="p.restartRequired" type="danger" size="small" effect="plain">
            改动会重启服务
          </el-tag>
          <el-tag v-else-if="p.reloadRequired" type="warning" size="small" effect="plain">
            改动会 reload
          </el-tag>

          <el-tag v-if="edits[p.name] !== undefined" type="success" size="small">已修改</el-tag>
          <el-tag v-if="unset.includes(p.name)" type="info" size="small">将恢复默认</el-tag>

          <!--
            「恢复默认」只对**被覆盖过的**参数有意义。对一个本来就取
            Pack 默认值的参数显示这个按钮，点下去什么也不会发生。
          -->
          <el-button
            v-if="ed.canUnset(p)"
            link
            type="primary"
            size="small"
            @click="ed.toggleUnset(p.name)"
          >
            {{ unset.includes(p.name) ? '取消' : '恢复默认' }}
          </el-button>
        </div>

        <div v-if="p.description" class="mt-1 text-xs text-gray-500">{{ p.description }}</div>

        <div class="mt-2 max-w-xl">
          <ParamField
            :param="p"
            :model-value="edits[p.name]"
            :disabled="unset.includes(p.name)"
            @update:model-value="(val) => ed.setValue(p.name, val)"
          />
        </div>
      </div>
    </el-card>

    <el-empty v-if="v && !v.params.length" description="这个角色没有可配置的参数" />

    <!-- 改动条：一直贴在底部，让「有几处没保存」始终看得见 -->
    <div
      v-if="changeCount > 0"
      class="sticky bottom-0 mt-4 flex items-center gap-3 border-t bg-white p-3 dark:bg-[#1d1e1f]"
    >
      <span class="text-sm">{{ changeCount }} 处改动未保存</span>
      <el-button size="small" @click="discard">放弃</el-button>
      <el-button size="small" type="primary" :loading="previewing" @click="doPreview">
        预览
      </el-button>
    </div>

    <!-- 预览即确认：先摆出会发生什么，再让人点 -->
    <el-dialog :model-value="preview !== null" title="确认保存" width="560" @close="preview = null">
      <template v-if="preview">
        <div v-if="!preview.changed?.length" class="text-gray-600">
          规格没有变化——这次保存不会改动任何机器。
        </div>
        <template v-else>
          <div class="mb-2">将影响 {{ preview.changed.length }} 个实例：</div>
          <div
            v-for="c in preview.changed"
            :key="c.role + c.node"
            class="font-mono text-xs text-gray-600"
          >
            {{ c.role }}@{{ c.node }} {{ c.from.slice(0, 12) }} → {{ c.to.slice(0, 12) }}
          </div>

          <el-alert
            class="mt-3"
            :type="preview.effect === 'restart' ? 'error' : preview.effect === 'reload' ? 'warning' : 'info'"
            :closable="false"
            show-icon
            :title="effectText[preview.effect] ?? preview.effect"
            :description="
              preview.effect === 'restart'
                ? `触发它的参数：${preview.restarted?.join(', ')}`
                : preview.effect === 'reload'
                  ? `触发它的参数：${preview.reloaded?.join(', ')}`
                  : ''
            "
          />
        </template>
        <el-alert
          v-for="w in preview.warnings ?? []"
          :key="w"
          type="warning"
          :closable="false"
          class="mt-2"
          :title="w"
        />
      </template>

      <template #footer>
        <el-button @click="preview = null">取消</el-button>
        <el-button
          type="primary"
          :loading="saving"
          :disabled="!preview?.changed?.length"
          @click="doSave"
        >
          {{ needsConfirm ? '我知道后果，保存' : '保存' }}
        </el-button>
      </template>
    </el-dialog>
  </AppShell>
</template>

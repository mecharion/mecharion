<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { api, apiQuery, ApiError, seg } from '../lib/api'
import type { ConfigGroupDetail, SetParamsResult, StatusView } from '../lib/api'

/**
 * 配置组的建 / 改 / 删。
 *
 * 模型里**不存在无名的 per-node 覆盖**（ADR-0021）：想让一台机器与众
 * 不同，就得有一个说得出名字的理由。这个面板是那条规则在界面上的样子。
 */
const props = defineProps<{
  component: string
  role: string
  status: StatusView | null
  groups: ConfigGroupDetail[]
}>()
const emit = defineEmits<{ changed: [] }>()

const open = ref(false)
const editing = ref<ConfigGroupDetail | null>(null)
const name = ref('')
const members = ref<string[]>([])
const preview = ref<SetParamsResult | null>(null)
const busy = ref('')
const error = ref('')

/** 本角色下的全部实例节点——组成员只能从这里挑。 */
const candidates = computed(() =>
  (props.status?.instances ?? [])
    .filter((i) => i.role === props.role)
    .map((i) => i.node)
    .sort(),
)

/**
 * 已被**别的**组占用的节点。
 *
 * 服务端会拒绝重叠（一个实例只属于一个组），但让用户填完再被拒是很差的
 * 体验。这里直接把它们禁掉，并说清它在哪个组——那正是服务端错误里的话。
 */
const takenBy = computed(() => {
  const m: Record<string, string> = {}
  for (const g of props.groups) {
    if (g.role !== props.role || g.name === editing.value?.name) continue
    for (const n of g.members) m[n] = g.name
  }
  return m
})

function startCreate() {
  editing.value = null
  name.value = ''
  members.value = []
  preview.value = null
  error.value = ''
  open.value = true
}

function startEdit(g: ConfigGroupDetail) {
  editing.value = g
  name.value = g.name
  members.value = [...g.members]
  preview.value = null
  error.value = ''
  open.value = true
}

watch([name, members], () => (preview.value = null), { deep: true })

const canGo = computed(() => name.value.trim() !== '' && members.value.length > 0)

function body(dryRun: boolean) {
  return {
    role: props.role,
    members: members.value,
    // 参数与路径绑定沿用原样：这个面板管的是**成员**。
    // 改组里的参数走参数表单（选中那个组即可）——两件事分开，
    // 免得一个「调整成员」的操作顺手改掉配置。
    params: editing.value?.params ?? {},
    paths: editing.value?.paths ?? {},
    dryRun,
  }
}

async function doPreview() {
  error.value = ''
  busy.value = 'preview'
  try {
    preview.value = await api.put<SetParamsResult>(
      `/components/${seg(props.component)}/groups/${seg(name.value.trim())}`,
      body(true),
    )
  } catch (e) {
    preview.value = null
    error.value = e instanceof ApiError ? e.message : String(e)
  } finally {
    busy.value = ''
  }
}

async function doSave() {
  busy.value = 'save'
  try {
    await api.put(`/components/${seg(props.component)}/groups/${seg(name.value.trim())}`, body(false))
    ElMessage.success(`已保存配置组 ${name.value}`)
    open.value = false
    emit('changed')
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : String(e)
  } finally {
    busy.value = ''
  }
}

// ── 删组 ────────────────────────────────────────────────────────────────

const removing = ref<ConfigGroupDetail | null>(null)
const removePreview = ref<SetParamsResult | null>(null)

/**
 * 删组**不是清理**，是一次真实的配置变更：成员会回落到角色级取值，
 * 配置文件会变，声明了 restartRequired 的参数会重启服务。
 * 因此先干跑，把影响面摆出来再问。
 */
async function startRemove(g: ConfigGroupDetail) {
  removing.value = g
  removePreview.value = null
  busy.value = 'removePreview'
  try {
    removePreview.value = await api.del<SetParamsResult>(
      `/components/${seg(props.component)}/groups/${seg(g.name)}` +
        apiQuery({ role: props.role, dryRun: 'true' }),
    )
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
    removing.value = null
  } finally {
    busy.value = ''
  }
}

async function doRemove() {
  if (!removing.value) return
  busy.value = 'remove'
  try {
    await api.del(
      `/components/${seg(props.component)}/groups/${seg(removing.value.name)}` +
        apiQuery({ role: props.role }),
    )
    ElMessage.success(`已删除配置组 ${removing.value.name}`)
    removing.value = null
    emit('changed')
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}

const effectText: Record<string, string> = {
  restart: '会重启服务',
  reload: '会触发 reload',
  none: '不需要重启或 reload',
}

function effectType(e?: string) {
  return e === 'restart' ? 'error' : e === 'reload' ? 'warning' : 'info'
}

const mine = computed(() => props.groups.filter((g) => g.role === props.role))
</script>

<template>
  <el-card shadow="never">
    <div class="mb-3 flex items-center gap-3">
      <span class="font-medium">配置组</span>
      <span class="text-xs text-gray-500">角色 {{ role }}</span>
      <el-button class="ml-auto" size="small" type="primary" plain @click="startCreate">
        新建配置组
      </el-button>
    </div>

    <el-empty v-if="!mine.length" :image-size="60">
      <template #description>
        <div class="text-sm text-gray-500">
          还没有配置组——该角色的全部实例都用角色级取值。<br />
          需要让某几台机器与众不同时建一个组，它会有名字、能枚举、能对比。
        </div>
      </template>
    </el-empty>

    <div v-for="g in mine" :key="g.name" class="mb-3 border-b pb-3 last:border-b-0">
      <div class="flex flex-wrap items-center gap-2">
        <span class="font-mono text-sm">{{ g.name }}</span>
        <el-tag size="small" type="info">{{ g.members.length }} 台</el-tag>
        <el-button link type="primary" size="small" @click="startEdit(g)">改成员</el-button>
        <el-button link type="danger" size="small" @click="startRemove(g)">删除</el-button>
      </div>
      <div class="mt-1 font-mono text-xs text-gray-600">{{ g.members.join(', ') }}</div>
      <div v-if="g.paths && Object.keys(g.paths).length" class="mt-1 text-xs text-gray-500">
        多盘绑定：
        <span v-for="(vols, k) in g.paths" :key="k" class="mr-3 font-mono">
          {{ k }} → {{ vols.join(', ') }}
        </span>
      </div>
    </div>
  </el-card>

  <!-- 建 / 改 -->
  <el-dialog v-model="open" :title="editing ? '修改成员' : '新建配置组'" width="560">
    <el-alert v-if="error" type="error" :closable="false" class="mb-3" :title="error" />

    <div class="mb-3 flex items-center gap-2">
      <span class="text-sm text-gray-500">组名</span>
      <el-input v-model="name" :disabled="editing !== null" class="w-64" placeholder="ssd-nodes" />
    </div>

    <div class="text-sm text-gray-500">成员</div>
    <el-checkbox-group v-model="members" class="mt-1">
      <div v-for="n in candidates" :key="n" class="py-0.5">
        <el-checkbox :value="n" :disabled="takenBy[n] !== undefined">
          <span class="font-mono">{{ n }}</span>
          <span v-if="takenBy[n]" class="ml-2 text-xs text-gray-400">
            已属于 {{ takenBy[n] }}——一个实例只能在一个组里
          </span>
        </el-checkbox>
      </div>
    </el-checkbox-group>

    <el-empty v-if="!candidates.length" description="该角色下没有实例" :image-size="50" />

    <div v-if="preview" class="mt-4">
      <div v-if="!preview.changed?.length" class="text-sm text-gray-600">
        规格没有变化——这次保存不会改动任何机器。
      </div>
      <template v-else>
        <div class="text-sm">将影响 {{ preview.changed.length }} 个实例：</div>
        <div
          v-for="c in preview.changed"
          :key="c.role + c.node"
          class="font-mono text-xs text-gray-600"
        >
          {{ c.role }}@{{ c.node }} {{ c.from.slice(0, 12) }} → {{ c.to.slice(0, 12) }}
        </div>
        <el-alert
          class="mt-2"
          :type="effectType(preview.effect)"
          :closable="false"
          show-icon
          :title="effectText[preview.effect] ?? preview.effect"
        />
      </template>
    </div>

    <template #footer>
      <el-button @click="open = false">取消</el-button>
      <el-button :disabled="!canGo" :loading="busy === 'preview'" @click="doPreview">
        预览
      </el-button>
      <el-button type="primary" :disabled="!preview" :loading="busy === 'save'" @click="doSave">
        保存
      </el-button>
    </template>
  </el-dialog>

  <!-- 删 -->
  <el-dialog
    :model-value="removing !== null"
    title="删除配置组"
    width="520"
    @close="removing = null"
  >
    <div class="text-sm">
      删掉 <span class="font-mono">{{ removing?.name }}</span> 之后，它的
      {{ removing?.members.length }} 台成员会**回落到角色级取值**。
    </div>
    <div class="mt-2 text-sm text-amber-700">
      这不是清理，是一次真实的配置变更：那些机器上的配置文件会变回去。
    </div>

    <div v-if="removePreview" class="mt-3">
      <div class="text-sm">将影响 {{ removePreview.changed?.length ?? 0 }} 个实例：</div>
      <div
        v-for="c in removePreview.changed ?? []"
        :key="c.role + c.node"
        class="font-mono text-xs text-gray-600"
      >
        {{ c.role }}@{{ c.node }} {{ c.from.slice(0, 12) }} → {{ c.to.slice(0, 12) }}
      </div>
      <el-alert
        class="mt-2"
        :type="effectType(removePreview.effect)"
        :closable="false"
        show-icon
        :title="effectText[removePreview.effect] ?? removePreview.effect"
      />
    </div>

    <template #footer>
      <el-button @click="removing = null">取消</el-button>
      <el-button
        type="danger"
        :disabled="!removePreview"
        :loading="busy === 'remove'"
        @click="doRemove"
      >
        删除
      </el-button>
    </template>
  </el-dialog>
</template>

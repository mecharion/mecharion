<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import AppShell from '../components/AppShell.vue'
import { usePolled } from '../lib/useApi'
import { api, ApiError } from '../lib/api'
import type { PacksView, PackView, NodeView, UploadResult } from '../lib/api'

const router = useRouter()

const packs = usePolled<PacksView>('/packs', 30000)
const nodes = usePolled<{ nodes: NodeView[] }>('/nodes', 30000)

const pack = ref<PackView | null>(null)
const component = ref('')
const profile = ref('')
const picked = ref<string[]>([])
const busy = ref('')
const error = ref('')
const preview = ref<{ instances?: string[]; warnings?: string[] } | null>(null)

// 组件名默认等于 Pack 名（与 CLI 一致）。同一个 Pack 部署多份要用户主动
// 改名——只有那时他才是知情的。
watch(pack, (p) => {
  component.value = p?.name ?? ''
  profile.value = ''
  picked.value = []
  preview.value = null
  error.value = ''
})

/**
 * 只有**在线**的节点能被选。
 *
 * 一台 `pending` 的机器还没人在上面执行过 join，一台 `offline` 的机器
 * 收不到下发——部署到它们身上会得到一个永远不收敛的实例，而症状
 * （「装了但一直没起来」）看起来像组件本身有问题。
 */
const selectable = computed(() =>
  (nodes.data.value?.nodes ?? []).map((n) => ({
    ...n,
    ok: n.status === 'online' && !n.cordoned && !n.revoked,
  })),
)

const reason = (n: (typeof selectable.value)[number]) => {
  if (n.status === 'pending') return '还没加入'
  if (n.status === 'offline') return '已掉线'
  if (n.revoked) return '证书已吊销'
  if (n.cordoned) return '已暂停调和'
  return ''
}

const canGo = computed(() => pack.value !== null && component.value !== '' && picked.value.length > 0)

function body(dryRun: boolean) {
  return {
    pack: pack.value?.name,
    component: component.value,
    profile: profile.value || undefined,
    nodes: picked.value,
    dryRun,
  }
}

async function doPreview() {
  error.value = ''
  busy.value = 'preview'
  try {
    preview.value = await api.post('/components', body(true))
  } catch (e) {
    preview.value = null
    error.value = e instanceof ApiError ? e.message : String(e)
  } finally {
    busy.value = ''
  }
}

async function doDeploy() {
  busy.value = 'deploy'
  try {
    await api.post('/components', body(false))
    ElMessage.success(`已部署 ${component.value}`)
    await router.push(`/components/${component.value}`)
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : String(e)
  } finally {
    busy.value = ''
  }
}

const alreadyDeployed = computed(() => (pack.value?.deployed ?? []).length > 0)

// ── 上传 Pack ───────────────────────────────────────────────────────────
//
// 这是**唯一一处新增的供应链入口**（23-web-ui §2.7）：其余写操作都只是
// 既有 API 的另一个入口，而这里让一份新的可执行内容进到 mechd。
// 因此界面上要把「上传 ≠ 部署」说清楚——它只是把包放进本地集合。

const uploading = ref(false)
const uploaded = ref<UploadResult | null>(null)
const uploadError = ref('')

async function onFile(ev: Event) {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = '' // 允许重传同一个文件
  if (!file) return

  uploadError.value = ''
  uploaded.value = null
  uploading.value = true
  try {
    uploaded.value = await api.upload<UploadResult>('/packs', file)
    ElMessage.success(`已收下 ${uploaded.value.name} ${uploaded.value.version}`)
    await packs.reload()
  } catch (e) {
    uploadError.value = e instanceof ApiError ? e.message : String(e)
  } finally {
    uploading.value = false
  }
}

function sizeText(n: number) {
  if (n < 1024) return `${n} B`
  const units = ['KiB', 'MiB', 'GiB']
  let v = n / 1024
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(1)} ${units[i]}`
}
</script>

<template>
  <AppShell>
    <h1 class="text-xl font-semibold">部署组件</h1>

    <el-alert v-if="error" type="error" :closable="false" class="mt-4" :title="error" />

    <!--
      解析不了的 Pack 要说出来。静默跳过会让「我明明把包放进去了，
      列表里却没有」变成一次无从查起的排查。
    -->
    <el-alert
      v-for="p in packs.data.value?.problems ?? []"
      :key="p.dir"
      type="warning"
      :closable="false"
      class="mt-3"
      :title="`Pack 目录 ${p.dir} 被跳过：${p.reason}`"
    />

    <el-card class="mt-4" shadow="never">
      <div class="flex flex-wrap items-center gap-3">
        <span class="font-medium">上传 Pack</span>
        <label>
          <input type="file" accept=".mpack" class="hidden" @change="onFile" />
          <el-button size="small" type="primary" plain :loading="uploading" tag="span">
            选择 .mpack 文件
          </el-button>
        </label>
        <span class="text-sm text-gray-500">
          上传只是把它放进本地 Pack 集合，**不会部署任何东西**。
        </span>
      </div>

      <el-alert
        v-if="uploadError"
        type="error"
        :closable="false"
        class="mt-3"
        show-icon
        title="这个包没有被收下"
      >
        <pre class="whitespace-pre-wrap font-mono text-xs">{{ uploadError }}</pre>
        <div class="mt-1 text-xs text-gray-500">
          被拒的包**什么都不会留下**——校验在临时目录里做，通过了才进集合。
        </div>
      </el-alert>

      <el-alert v-if="uploaded" type="success" :closable="false" class="mt-3" show-icon>
        <template #title>
          已收下 {{ uploaded.name }} {{ uploaded.version }}-{{ uploaded.revision }}
          <span v-if="uploaded.replaced" class="text-amber-700">（覆盖了同名同版本的一份）</span>
        </template>
        <div class="font-mono text-xs text-gray-600">
          sha256:{{ uploaded.digest.slice(0, 16) }}… · {{ sizeText(uploaded.size) }}
        </div>
        <div v-for="w in uploaded.warnings ?? []" :key="w" class="text-xs text-amber-700">
          警告：{{ w }}
        </div>
      </el-alert>
    </el-card>

    <el-card class="mt-4" shadow="never" v-loading="packs.loading.value">
      <div class="mb-3 font-medium">① 选 Pack</div>
      <el-table
        :data="packs.data.value?.packs ?? []"
        row-key="name"
        highlight-current-row
        @current-change="(row: PackView) => (pack = row)"
      >
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column prop="description" label="说明" min-width="240" />
        <el-table-column label="最新版本" width="120">
          <template #default="{ row }">{{ row.versions?.[0] }}</template>
        </el-table-column>
        <el-table-column label="已部署" min-width="160">
          <template #default="{ row }">
            <span v-if="row.deployed?.length" class="text-gray-600">
              {{ row.deployed.join(', ') }}
            </span>
            <span v-else class="text-gray-300">—</span>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <template v-if="pack">
      <el-card class="mt-4" shadow="never">
        <div class="mb-3 font-medium">② 组件名与形态</div>
        <div class="flex flex-wrap items-center gap-4">
          <div class="flex items-center gap-2">
            <span class="text-sm text-gray-500">组件名</span>
            <el-input v-model="component" class="w-56" />
          </div>
          <div v-if="pack.profiles?.length" class="flex items-center gap-2">
            <span class="text-sm text-gray-500">形态</span>
            <el-select v-model="profile" class="w-40" placeholder="默认">
              <el-option label="默认" value="" />
              <el-option v-for="pf in pack.profiles" :key="pf" :label="pf" :value="pf" />
            </el-select>
          </div>
        </div>
        <div v-if="alreadyDeployed" class="mt-2 text-sm text-amber-700">
          这个 Pack 已经部署过（{{ pack.deployed?.join(', ') }}）。
          再部署一份要换一个组件名，否则会被当成更新现有组件而遭拒。
        </div>
      </el-card>

      <el-card class="mt-4" shadow="never">
        <div class="mb-3 font-medium">③ 选节点</div>
        <el-table :data="selectable" row-key="name" @selection-change="
          (rows: any[]) => (picked = rows.map((r) => r.name))
        ">
          <el-table-column type="selection" width="46" :selectable="(row: any) => row.ok" />
          <el-table-column prop="name" label="节点" min-width="140" />
          <el-table-column label="状态" width="140">
            <template #default="{ row }">
              <el-tag :type="row.ok ? 'success' : 'info'" size="small">
                {{ row.ok ? '可用' : reason(row) }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column prop="address" label="地址" min-width="140" />
        </el-table>
      </el-card>

      <div class="mt-4 flex items-center gap-3">
        <el-button :disabled="!canGo" :loading="busy === 'preview'" @click="doPreview">
          预览
        </el-button>
        <el-button
          type="primary"
          :disabled="!preview"
          :loading="busy === 'deploy'"
          @click="doDeploy"
        >
          部署
        </el-button>
        <span class="text-sm text-gray-500">已选 {{ picked.length }} 台</span>
      </div>

      <el-card v-if="preview" class="mt-4" shadow="never">
        <div class="mb-2 font-medium">将创建 {{ preview.instances?.length ?? 0 }} 个实例</div>
        <div v-for="k in preview.instances ?? []" :key="k" class="font-mono text-xs text-gray-600">
          {{ k }}
        </div>
        <el-alert
          v-for="w in preview.warnings ?? []"
          :key="w"
          type="warning"
          :closable="false"
          class="mt-2"
          :title="w"
        />
        <div class="mt-3 text-sm text-gray-500">
          部署之后可以在组件的「参数」页调整配置——这里只做最小可用部署，
          其余参数取 Pack 默认值。
        </div>
      </el-card>
    </template>
  </AppShell>
</template>

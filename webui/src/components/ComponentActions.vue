<script setup lang="ts">
import { computed, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, ApiError, seg } from '../lib/api'
import type { PacksView, RemovalImpact, RemoveResult, StatusView } from '../lib/api'

const props = defineProps<{ status: StatusView | null }>()
const emit = defineEmits<{ changed: [] }>()

const name = computed(() => props.status?.component ?? '')
const busy = ref('')

/** 升级对话框 */
const upgradeOpen = ref(false)
const versions = ref<string[]>([])
const target = ref('')
const preview = ref<{ instances?: string[]; warnings?: string[] } | null>(null)

async function openUpgrade() {
  upgradeOpen.value = true
  preview.value = null
  target.value = ''
  try {
    const packs = await api.get<PacksView>('/packs')
    const p = packs.packs.find((x) => x.name === props.status?.pack)
    // 只列**当前版本之外**的：把当前版本摆进去，用户选它之后什么也不会
    // 发生，而界面已经答应了「要升级」——那种落空最让人困惑
    versions.value = (p?.versions ?? []).filter((x) => x !== props.status?.version)
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  }
}

/**
 * 升级先干跑。
 *
 * 与改配置同一条：确认要问的是「会发生什么」，而那句话得先算出来。
 * 升级比改配置更该这样——它会逐批重启整个组件。
 */
async function previewUpgrade() {
  if (!target.value) return
  busy.value = 'preview'
  try {
    preview.value = await api.post(`/components/${seg(name.value)}/upgrade`, {
      version: target.value,
      dryRun: true,
    })
  } catch (e) {
    preview.value = null
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}

async function doUpgrade() {
  busy.value = 'upgrade'
  try {
    await api.post(`/components/${seg(name.value)}/upgrade`, { version: target.value })
    ElMessage.success(`已开始升级到 ${target.value}，按批次滚动进行`)
    upgradeOpen.value = false
    emit('changed')
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}

/**
 * 回滚要二档确认：输入组件名。
 *
 * 10-cli §7 的分档在这里对齐——回滚是**真的把机器换回旧版本**，
 * 一个 y/N 与它的影响面不相称。
 */
async function doRollback() {
  try {
    await ElMessageBox.prompt(
      `回滚会把 ${name.value} 的全部实例换回上一个版本。请输入组件名确认：`,
      '确认回滚',
      {
        inputValidator: (val: string) => val === name.value || '组件名不匹配',
        confirmButtonText: '回滚',
        type: 'warning',
      },
    )
  } catch {
    return // 用户取消
  }
  busy.value = 'rollback'
  try {
    await api.post(`/components/${seg(name.value)}/rollback`, {})
    ElMessage.success('已开始回滚')
    emit('changed')
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}

async function setRunState(state: 'stop' | 'start') {
  const label = state === 'stop' ? '停止' : '启动'
  if (state === 'stop') {
    try {
      await ElMessageBox.confirm(
        `这会停掉 ${name.value} 的全部实例。**期望状态**随之改变——` +
          `调和器不会再把它们拉起来，直到你显式启动。`,
        `确认${label}`,
        { type: 'warning', confirmButtonText: label },
      )
    } catch {
      return
    }
  }
  busy.value = state
  try {
    await api.post(`/components/${seg(name.value)}/${state}`, {})
    ElMessage.success(`已${label}`)
    emit('changed')
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}

const anyStopped = computed(() =>
  (props.status?.instances ?? []).some((i) => i.runState === 'stopped'),
)

/* ── 移除 ──────────────────────────────────────────────────────────────
 *
 * 这是整个界面上唯一真正销毁东西的操作，因此它的形态与 CLI 完全对齐
 * （10-cli §4.3 / §7）：**先把后果算出来给人看，再要一个打不错的确认**。
 *
 * 一个只问「确定要删 pg-main 吗」的弹窗没有信息量——用户不知道会动几台
 * 机器、哪些目录会没、哪些会留下来。
 */
const removeOpen = ref(false)
const impact = ref<RemovalImpact | null>(null)
const purgeData = ref(false)
const keepConfig = ref(false)

/** 打开对话框时先干跑一次；开关变了要重算，否则显示的是一个不会发生的结果。 */
async function refreshImpact() {
  busy.value = 'impact'
  try {
    const res = await api.del<RemoveResult>(`/components/${name.value}`, {
      dryRun: true,
      purgeData: purgeData.value,
      keepConfig: keepConfig.value,
    })
    impact.value = res.impact
  } catch (e) {
    impact.value = null
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}

async function openRemove() {
  removeOpen.value = true
  purgeData.value = false
  keepConfig.value = false
  impact.value = null
  await refreshImpact()
}

async function doRemove() {
  // ① 二档确认：把组件名打出来。
  //    与 CLI 同一档——它挡的是「删错了对象」，不是「手滑点了确定」。
  try {
    await ElMessageBox.prompt(
      `移除会在 ${impact.value?.nodes?.length ?? 0} 台机器上停掉进程、删掉目录，` +
        `不可撤销。请输入组件名确认：`,
      '确认移除',
      {
        inputValidator: (val: string) => val === name.value || '组件名不匹配',
        confirmButtonText: '移除',
        type: 'warning',
      },
    )
  } catch {
    return
  }

  // ② --purge-data 再确认一次（10-cli §7 的表：组件名 ＋ 单独确认删数据）。
  //    删掉进程与配置可以重新部署回来，删掉数据不能——两者不该由同一个
  //    动作买断。
  if (purgeData.value) {
    try {
      await ElMessageBox.confirm(
        `还会删除 ${impact.value?.deleted?.length ?? 0} 个目录里的**数据**，` +
          `这一步没有任何撤销手段。确定？`,
        '确认删除数据',
        { type: 'error', confirmButtonText: '连数据一起删' },
      )
    } catch {
      return
    }
  }

  busy.value = 'remove'
  try {
    const res = await api.del<RemoveResult>(`/components/${name.value}`, {
      confirm: name.value,
      purgeData: purgeData.value,
      keepConfig: keepConfig.value,
    })
    removeOpen.value = false
    ElMessage.success(
      res.deleted ? `${name.value} 已移除` : `已下发移除意图，${name.value} 正在移除中`,
    )
    emit('changed')
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}
</script>

<template>
  <div class="flex flex-wrap items-center gap-2">
    <el-button size="small" @click="openUpgrade">升级</el-button>
    <el-button size="small" :loading="busy === 'rollback'" @click="doRollback">回滚</el-button>
    <el-button
      v-if="anyStopped"
      size="small"
      type="success"
      :loading="busy === 'start'"
      @click="setRunState('start')"
    >
      启动
    </el-button>
    <el-button
      v-else
      size="small"
      :loading="busy === 'stop'"
      @click="setRunState('stop')"
    >
      停止
    </el-button>
    <router-link :to="`/components/${name}/params`">
      <el-button size="small" type="primary" plain>参数</el-button>
    </router-link>
    <!-- 移除放在最右且用 danger：它与旁边那些可撤销的操作不是一类东西 -->
    <el-button size="small" type="danger" plain @click="openRemove">移除</el-button>
  </div>

  <el-dialog v-model="upgradeOpen" title="升级" width="520">
    <div class="flex items-center gap-3">
      <span class="text-sm text-gray-500">当前 {{ status?.version }}</span>
      <span>→</span>
      <el-select v-model="target" placeholder="选择目标版本" class="w-48" @change="preview = null">
        <el-option v-for="ver in versions" :key="ver" :label="ver" :value="ver" />
      </el-select>
      <el-button size="small" :disabled="!target" :loading="busy === 'preview'" @click="previewUpgrade">
        预览
      </el-button>
    </div>

    <el-empty v-if="!versions.length" description="本地没有其它版本的 Pack" :image-size="60" />

    <div v-if="preview" class="mt-4">
      <div class="text-sm">将影响 {{ preview.instances?.length ?? 0 }} 个实例：</div>
      <div v-for="k in preview.instances ?? []" :key="k" class="font-mono text-xs text-gray-600">
        {{ k }}
      </div>
      <!--
        分批与健康门禁是 M7 的行为，用户在这里必须知道它——
        否则「点了升级，怎么只动了一台」看起来像卡住了
      -->
      <el-alert
        class="mt-3"
        type="warning"
        :closable="false"
        show-icon
        title="升级按批次滚动进行"
        description="每批之间要通过健康门禁（收敛、健康、无重启、稳定 30 秒）才放行下一批。任一批失败会停下来等人处理。"
      />
      <el-alert
        v-for="w in preview.warnings ?? []"
        :key="w"
        type="warning"
        :closable="false"
        class="mt-2"
        :title="w"
      />
    </div>

    <template #footer>
      <el-button @click="upgradeOpen = false">取消</el-button>
      <el-button
        type="primary"
        :disabled="!preview"
        :loading="busy === 'upgrade'"
        @click="doUpgrade"
      >
        开始升级
      </el-button>
    </template>
  </el-dialog>

  <el-dialog v-model="removeOpen" title="移除组件" width="600">
    <el-alert
      type="error"
      :closable="false"
      show-icon
      title="这是整个工具里最危险的操作"
      description="它会在每台机器上停掉进程、卸掉工作负载、删掉目录。不可撤销。"
    />

    <div v-if="impact" class="mt-4 text-sm">
      <div class="mb-2">
        将移除 <b>{{ impact.instances }}</b> 个实例，分布在
        <span class="font-mono">{{ (impact.nodes ?? []).join(' ') }}</span>
      </div>

      <!--
        两边都列。只列要删的，人无从知道盘上还会剩什么；只列要留的，
        人无从判断这次删除到底有多狠。
      -->
      <div v-if="impact.deleted?.length" class="mt-3">
        <div class="text-red-600">会删掉（每台机器上）：</div>
        <div v-for="d in impact.deleted" :key="d" class="font-mono text-xs text-gray-600">
          {{ d }}
        </div>
      </div>
      <div v-if="impact.retained?.length" class="mt-3">
        <div class="text-green-700">会保留（登记为孤儿，可在「孤儿」页清理）：</div>
        <div v-for="d in impact.retained" :key="d" class="font-mono text-xs text-gray-600">
          {{ d }}
        </div>
      </div>

      <div class="mt-4 flex flex-col gap-1">
        <el-checkbox v-model="purgeData" @change="refreshImpact">
          连数据目录一起删除（默认保留）
        </el-checkbox>
        <el-checkbox v-model="keepConfig" @change="refreshImpact">
          保留配置目录（默认删除）
        </el-checkbox>
      </div>

      <el-alert
        class="mt-3"
        type="info"
        :closable="false"
        show-icon
        title="移除是逐节点异步完成的"
        description="确认之后卸载才开始下发，组件会停在「正在移除」直到全部实例报告拆干净。期间它不接受任何其它写操作。"
      />
    </div>

    <template #footer>
      <el-button @click="removeOpen = false">取消</el-button>
      <el-button
        type="danger"
        :disabled="!impact"
        :loading="busy === 'remove'"
        @click="doRemove"
      >
        移除
      </el-button>
    </template>
  </el-dialog>
</template>

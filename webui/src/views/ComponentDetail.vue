<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import AppShell from '../components/AppShell.vue'
import ComponentActions from '../components/ComponentActions.vue'
import { usePolled } from '../lib/useApi'
import { useLive } from '../lib/useLive'
import { seg, type StatusView, type RolloutView, type InstanceView, type WatchSnapshot } from '../lib/api'

const route = useRoute()
const name = route.params.name as string

const status = usePolled<StatusView>(`/components/${seg(name)}/status`, 4000)
// 从没升级过的组件上 rollout 会报错——那不是故障，只是没有可看的
const rollout = usePolled<RolloutView>(`/components/${seg(name)}/rollout`, 4000)

/**
 * SSE 实时流（23-web-ui §4.5）。
 *
 * 它**推完整状态**，因此这里的处理是「有就用它」，没有才回落到轮询那份。
 * 轮询代码刻意留着：SSE 被反向代理吃掉时页面仍然是对的，只是慢一点
 * ——实时性是一个优化，不是一个依赖。
 */
const live = useLive<WatchSnapshot>(`/components/${seg(name)}/watch`, {
  onUp: () => {
    // 接上了就停轮询：两边都在拉等于白费一半请求
    status.stop()
    rollout.stop()
  },
  onDown: () => {
    // 断了就把轮询开回去，并立刻拉一次——别让页面停在断线那一刻
    status.start()
    rollout.start()
    void status.reload()
    void rollout.reload()
  },
})
live.start()

const st = computed(() => live.data.value?.status ?? status.data.value)
const ro = computed(() => {
  if (live.data.value) return live.data.value.rollout ?? null
  return rollout.error.value ? null : rollout.data.value
})
const hasDrift = computed(() => (st.value?.instances ?? []).some((i) => i.drift?.length))

/**
 * 收敛那一列要分三种，不是两种。
 *
 * 「在排队」与「出了问题」在收敛上都是「否」，而一个只要等、一个要人去看
 * ——这正是 pendingVersion 存在的理由（22-multi-node §6.8）。
 */
function convergence(i: InstanceView): { text: string; type: 'success' | 'info' | 'danger' } {
  if (i.converged) return { text: '已收敛', type: 'success' }
  if (i.pendingVersion) return { text: `排队中（${i.pendingVersion}）`, type: 'info' }
  return { text: '未收敛', type: 'danger' }
}

const stateText: Record<string, string> = {
  running: '进行中',
  succeeded: '已成功',
  failed: '已失败',
  halted: '已因故障停下',
  paused: '已冻结判定',
  aborted: '已中止并回退',
}

function stateType(s: string) {
  if (s === 'succeeded') return 'success'
  if (s === 'failed' || s === 'halted') return 'danger'
  if (s === 'paused') return 'warning'
  return 'info'
}

function short(d?: string) {
  return d ? d.slice(0, 12) : ''
}
</script>

<template>
  <AppShell>
    <div class="flex items-baseline gap-3">
      <h1 class="text-xl font-semibold">{{ name }}</h1>
      <span v-if="st" class="text-gray-500">
        {{ st.pack }} {{ st.version }}
        <template v-if="st.profile">，形态 {{ st.profile }}</template>
      </span>
      <!--
        实时状态。`polling` **不是错误**——它是降级但正确，因此用最不
        显眼的样式。做成红色警告会让人以为界面坏了，而它只是慢了几秒。
      -->
      <span class="ml-auto text-xs text-gray-400">
        <template v-if="live.state.value === 'live'">● 实时</template>
        <template v-else-if="live.state.value === 'polling'">○ 轮询（每 5 秒）</template>
        <template v-else>○ 连接中</template>
      </span>
      <el-tag v-if="st" :type="st.converged ? 'success' : 'warning'">
        {{ st.converged ? '已收敛' : '未收敛' }}
      </el-tag>
    </div>

    <ComponentActions class="mt-4" :status="st" @changed="status.reload()" />

    <el-alert
      v-if="status.error.value"
      type="error"
      :closable="false"
      class="mt-4"
      :title="status.error.value"
    />

    <!-- 进行中/停下的变更放最上面：它是此刻最要紧的信息 -->
    <el-card v-if="ro" class="mt-4" shadow="never">
      <div class="flex flex-wrap items-center gap-3">
        <span class="font-medium">{{ ro.kind }} {{ ro.from || '—' }} → {{ ro.to }}</span>
        <el-tag :type="stateType(ro.state)" size="small">
          {{ stateText[ro.state] ?? ro.state }}
        </el-tag>
        <span v-if="ro.batches" class="text-sm text-gray-500">
          第 {{ ro.batch }}/{{ ro.batches }} 批
          <template v-if="ro.current">（正在做 {{ ro.current }}）</template>
        </span>
      </div>

      <div v-if="ro.reason" class="mt-2 text-sm text-gray-600">{{ ro.reason }}</div>

      <!-- 停下之后集群是混合版本的，不列出来运维得一台台去看 -->
      <div v-if="ro.mixed" class="mt-3 text-sm">
        <div class="text-gray-500">当前分布</div>
        <div v-for="(nodes, ver) in ro.mixed" :key="ver" class="mt-1">
          <el-tag size="small" class="mr-2">{{ ver }}</el-tag>
          <span class="text-gray-600">{{ nodes.join(', ') }}</span>
        </div>
      </div>

      <div v-if="ro.skipped?.length" class="mt-3 text-sm text-gray-600">
        已跳过 {{ ro.skipped.join(', ') }}（被 cordon，用 node uncordon 恢复）
      </div>
    </el-card>

    <el-alert
      v-for="w in st?.warnings ?? []"
      :key="w"
      type="warning"
      :closable="false"
      class="mt-3"
      :title="w"
    />

    <el-card class="mt-4" shadow="never" v-loading="status.loading.value && !st">
      <el-table :data="st?.instances ?? []" row-key="node">
        <el-table-column prop="role" label="角色" width="110" />
        <el-table-column prop="node" label="节点" min-width="130" />
        <el-table-column prop="ordinal" label="序号" width="70" align="right" />

        <el-table-column label="工作负载" width="140">
          <template #default="{ row }">
            <span>{{ row.workload || '—' }}</span>
            <!--
              重启次数要显示：一个每分钟崩一次又被拉起的服务，
              在「running / healthy」两列里与健康的没有区别
            -->
            <el-tag v-if="row.restarts" type="warning" size="small" class="ml-1">
              重启 {{ row.restarts }}
            </el-tag>
          </template>
        </el-table-column>

        <el-table-column label="健康" width="100">
          <template #default="{ row }">
            <el-tag
              v-if="row.health"
              size="small"
              :type="row.health === 'healthy' ? 'success' : 'danger'"
            >
              {{ row.health }}
            </el-tag>
            <span v-else class="text-gray-300">—</span>
          </template>
        </el-table-column>

        <el-table-column label="收敛" min-width="160">
          <template #default="{ row }">
            <el-tag size="small" :type="convergence(row).type">
              {{ convergence(row).text }}
            </el-tag>
            <el-tag v-if="row.rolledBack" type="danger" size="small" class="ml-1">
              已回滚
            </el-tag>
          </template>
        </el-table-column>

        <el-table-column label="期望 / 实际" min-width="190">
          <template #default="{ row }">
            <span v-if="row.got && row.got !== row.want" class="text-xs text-gray-500">
              {{ short(row.want) }} / {{ short(row.got) }}
            </span>
            <span v-else class="text-xs text-gray-400">{{ short(row.want) }}</span>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <!-- 漂移单列一块：它是「机器上被人改过」，与收敛不是一回事 -->
    <el-card v-if="hasDrift" class="mt-4" shadow="never">
      <div class="mb-3 font-medium">检出的漂移</div>
      <template v-for="i in st?.instances ?? []" :key="i.node">
        <div v-if="i.drift?.length" class="mb-4">
          <div class="text-sm text-gray-500">{{ i.role }}@{{ i.node }}</div>
          <div v-for="d in i.drift" :key="d.resource" class="ml-3 mt-1">
            <span class="font-mono text-sm">{{ d.resource }}</span>
            <!--
              **策略要和资源写在一起**：看到漂移的第一个问题是「它会不会被
              改回去」，而答案取决于 driftPolicy（含站点覆盖）
            -->
            <el-tag size="small" class="ml-2" type="info">{{ d.policy || '—' }}</el-tag>
            <div v-if="d.detail" class="text-xs text-gray-500">{{ d.detail }}</div>
          </div>
          <div v-if="i.suppressed?.length" class="ml-3 mt-1 text-xs text-gray-400">
            已抑制：{{ i.suppressed.join(', ') }}
          </div>
        </div>
      </template>
    </el-card>
  </AppShell>
</template>

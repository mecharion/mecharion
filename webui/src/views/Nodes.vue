<script setup lang="ts">
import { ElMessage, ElMessageBox } from 'element-plus'
import AppShell from '../components/AppShell.vue'
import { api, ApiError, seg } from '../lib/api'
import { ref } from 'vue'
import { usePolled } from '../lib/useApi'
import type { NodeView } from '../lib/api'

const { data, error, loading, reload } = usePolled<{ nodes: NodeView[] }>('/nodes')

const busy = ref('')

/**
 * cordon / uncordon **不确认**：它可逆，而且 `调和` 那一列上立刻看得见。
 * 每个动作都拦一下会训练用户无脑点确定，而下一次是 revoke。
 */
async function toggleCordon(n: NodeView) {
  const verb = n.cordoned ? 'uncordon' : 'cordon'
  busy.value = n.name
  try {
    await api.post(`/nodes/${seg(n.name)}/${verb}`)
    ElMessage.success(n.cordoned ? `已恢复调和 ${n.name}` : `已暂停调和 ${n.name}`)
    await reload()
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}

/** revoke 走一档：它让那台机器立刻连不上，但没有丢数据。 */
async function revoke(n: NodeView) {
  try {
    await ElMessageBox.confirm(
      `吊销 ${n.name} 的证书之后它会立刻连不上。

` +
        `注意：证书的 TLS 握手**仍会成功**，被拒的是随后的每一个 RPC` +
        `——这是应用层吊销的代价（ADR-0034）。`,
      '确认吊销证书',
      { type: 'warning', confirmButtonText: '吊销' },
    )
  } catch {
    return
  }
  busy.value = n.name
  try {
    await api.post(`/nodes/${seg(n.name)}/revoke`)
    ElMessage.success(`已吊销 ${n.name}`)
    await reload()
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}

/**
 * remove 走**二档**（输入节点名）。
 *
 * 它不会去那台机器上卸载任何东西。删掉一个还跑着组件的节点，那些组件会
 * 从中心的视图里消失而仍在机器上跑——那是最难清理的一类状态。
 */
async function remove(n: NodeView) {
  try {
    await ElMessageBox.prompt(
      `将从册子上移除 ${n.name}。**机器上的东西不会被卸载**——` +
        `仍在跑的组件会从这里消失，但它们还在那台机器上。

请输入节点名确认：`,
      '确认移除节点',
      {
        inputValidator: (v: string) => v === n.name || '节点名不匹配',
        confirmButtonText: '移除',
        type: 'warning',
      },
    )
  } catch {
    return
  }
  busy.value = n.name
  try {
    await api.del(`/nodes/${seg(n.name)}`)
    ElMessage.success(`已移除 ${n.name}`)
    await reload()
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}

/**
 * 三个状态在**要人做的事**上完全不同，因此颜色也不能一样：
 *
 *   online   在线
 *   offline  来过，现在掉了——去查它为什么掉了（危险，不是「信息」）
 *   pending  登记了，还没人去那台机器上执行 join——去装它
 */
const statusText: Record<string, string> = {
  online: '在线',
  offline: '已掉线',
  pending: '待加入',
}

function statusType(s: string) {
  if (s === 'online') return 'success'
  if (s === 'offline') return 'danger'
  return 'warning'
}
</script>

<template>
  <AppShell>
    <div class="flex items-center gap-3">
      <h1 class="text-xl font-semibold">节点</h1>
      <router-link to="/join" class="ml-auto">
        <el-button size="small" type="primary" plain>加入节点</el-button>
      </router-link>
    </div>

    <el-alert v-if="error" type="error" :closable="false" class="mt-4" :title="error" />

    <el-card class="mt-4" shadow="never" v-loading="loading && !data">
      <el-empty v-if="data && !data.nodes?.length" description="还没有节点" />
      <el-table v-else :data="data?.nodes ?? []" row-key="name">
        <el-table-column prop="name" label="名称" min-width="140" />
        <el-table-column prop="address" label="地址" min-width="140" />
        <el-table-column label="状态" width="110">
          <template #default="{ row }">
            <el-tag :type="statusType(row.status)" size="small">
              {{ statusText[row.status] ?? row.status }}
            </el-tag>
          </template>
        </el-table-column>

        <!--
          cordon 与 revoke **各占一列**，不并进状态。
          一台机器可以既在线又被暂停调和——并成一列会把这个信息丢掉。
        -->
        <el-table-column label="调和" width="110">
          <template #default="{ row }">
            <el-tag v-if="row.cordoned" type="warning" size="small">已暂停</el-tag>
            <span v-else class="text-gray-300">—</span>
          </template>
        </el-table-column>
        <el-table-column label="证书" width="110">
          <template #default="{ row }">
            <el-tag v-if="row.revoked" type="danger" size="small">已吊销</el-tag>
            <span v-else class="text-gray-300">—</span>
          </template>
        </el-table-column>

        <el-table-column label="" width="210">
          <template #default="{ row }">
            <el-button
              link
              size="small"
              :loading="busy === row.name"
              @click="toggleCordon(row)"
            >
              {{ row.cordoned ? '恢复调和' : '暂停调和' }}
            </el-button>
            <el-button
              v-if="!row.revoked"
              link
              type="warning"
              size="small"
              @click="revoke(row)"
            >
              吊销证书
            </el-button>
            <el-button link type="danger" size="small" @click="remove(row)">移除</el-button>
          </template>
        </el-table-column>

        <el-table-column label="标签" min-width="160">
          <template #default="{ row }">
            <el-tag
              v-for="(v, k) in row.labels ?? {}"
              :key="k"
              size="small"
              class="mr-1"
              type="info"
            >
              {{ k }}={{ v }}
            </el-tag>
            <span v-if="!row.labels" class="text-gray-300">—</span>
          </template>
        </el-table-column>

        <!--
          孤儿必须在**节点**这一层显示：mechd 里可能已经没有那个 Component 了，
          组件页根本查不到它。
        -->
        <el-table-column label="孤儿实例" width="110">
          <template #default="{ row }">
            <el-tooltip v-if="row.orphans?.length" placement="left">
              <template #content>
                <div v-for="o in row.orphans" :key="o.instance">
                  {{ o.instance }} —— 孤立于 {{ o.firstSeen?.slice(0, 10) }}
                </div>
              </template>
              <el-tag type="warning" size="small">{{ row.orphans.length }}</el-tag>
            </el-tooltip>
            <span v-else class="text-gray-300">—</span>
          </template>
        </el-table-column>
      </el-table>
    </el-card>
  </AppShell>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import AppShell from '../components/AppShell.vue'
import { api, ApiError, seg } from '../lib/api'
import type { OrphanEntry } from '../lib/api'
import { canPurge, isInstalled, since } from '../lib/orphans'

/*
 * 孤儿页是 `component remove` 保留数据目录之后的**配套**：
 * 10-cli §4.3 明写「保留而不提供发现机制等于把问题推给未来」。
 * 没有这一页，默认保留就等于在每台机器上悄悄堆下没人知道来历的目录。
 */

const list = ref<OrphanEntry[]>([])
const loading = ref(false)
const busy = ref('')

async function load() {
  loading.value = true
  try {
    const res = await api.get<{ orphans: OrphanEntry[] }>('/orphans')
    list.value = res.orphans ?? []
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    loading.value = false
  }
}
onMounted(load)

const key = (o: OrphanEntry) => `${o.node}/${o.instance}`

async function purge(o: OrphanEntry) {
  // 二档确认：与 CLI 同一档。它删的是真实数据，而且没有任何撤销。
  try {
    await ElMessageBox.prompt(
      `将从 ${o.node} 上永久删除以下目录：\n${(o.paths ?? []).join('\n')}\n\n请输入实例键确认：`,
      '确认清理',
      {
        inputValidator: (val: string) => val === o.instance || '实例键不匹配',
        confirmButtonText: '清理',
        type: 'error',
      },
    )
  } catch {
    return
  }
  busy.value = key(o)
  try {
    await api.del(`/orphans/${seg(o.node)}/${seg(o.instance)}`, { confirm: o.instance })
    ElMessage.success(`已下发清理意图；${o.node} 下次连上来时执行`)
    await load()
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  } finally {
    busy.value = ''
  }
}

const empty = computed(() => !loading.value && list.value.length === 0)
</script>

<template>
  <AppShell>
    <div class="mb-4 flex items-center justify-between">
      <h1 class="text-xl font-semibold">孤儿</h1>
      <el-button size="small" :loading="loading" @click="load">刷新</el-button>
    </div>

    <el-alert
      type="info"
      :closable="false"
      show-icon
      title="孤儿永不自动清理"
      description="「控制面少发了一条」与「用户真的删了这个组件」在节点侧分辨不出来，而卸载不可逆。因此这里的每一条都要人来决定。"
      class="mb-4"
    />

    <el-empty v-if="empty" description="没有孤儿" />

    <el-table v-else v-loading="loading" :data="list" size="small">
      <el-table-column prop="node" label="节点" width="140" />
      <el-table-column prop="instance" label="实例" width="200">
        <template #default="{ row }">
          <span class="font-mono text-xs">{{ row.instance }}</span>
        </template>
      </el-table-column>
      <el-table-column label="类型" width="150">
        <template #default="{ row }">
          <el-tag v-if="isInstalled(row)" type="warning" size="small">仍装着</el-tag>
          <el-tag v-else type="info" size="small">数据残留</el-tag>
          <el-tag v-if="row.purgeRequested" type="danger" size="small" class="ml-1">
            待清理
          </el-tag>
        </template>
      </el-table-column>
      <el-table-column label="存在" width="80">
        <template #default="{ row }">{{ since(row.firstSeen) }}</template>
      </el-table-column>
      <el-table-column label="路径">
        <template #default="{ row }">
          <div v-if="isInstalled(row)" class="text-xs text-gray-500">
            整个实例，不只是目录——清理它要先重新纳管，或登机手工处理
          </div>
          <div v-for="p in row.paths ?? []" :key="p" class="font-mono text-xs text-gray-600">
            {{ p }}
          </div>
        </template>
      </el-table-column>
      <el-table-column label="" width="90">
        <template #default="{ row }">
          <el-button
            v-if="canPurge(row)"
            size="small"
            type="danger"
            plain
            :loading="busy === `${row.node}/${row.instance}`"
            @click="purge(row)"
          >
            清理
          </el-button>
        </template>
      </el-table-column>
    </el-table>
  </AppShell>
</template>

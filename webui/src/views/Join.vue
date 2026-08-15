<script setup lang="ts">
import { computed, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import AppShell from '../components/AppShell.vue'
import { usePolled } from '../lib/useApi'
import { api, ApiError, seg } from '../lib/api'
import { fillJoinHost, isLocalOrigin } from '../lib/joinCommand'
import type { JoinTokenView } from '../lib/api'

const tokens = usePolled<{ tokens: JoinTokenView[] }>('/nodes/tokens', 15000)

// 创建表单
const node = ref('')
const ttl = ref('30m')
const uses = ref(1)
const creating = ref(false)
const error = ref('')

/** 刚创建出来的那一张。**关掉就没了**——存的是哈希。 */
const fresh = ref<JoinTokenView | null>(null)

const origin = window.location.origin
const localWarning = computed(() => isLocalOrigin(origin))
const command = computed(() => fillJoinHost(fresh.value?.joinCommand ?? '', origin))

async function create() {
  error.value = ''
  creating.value = true
  try {
    fresh.value = await api.post<JoinTokenView>('/nodes/tokens', {
      node: node.value || undefined,
      ttl: ttl.value,
      uses: uses.value,
    })
    await tokens.reload()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : String(e)
  } finally {
    creating.value = false
  }
}

async function copy() {
  try {
    await navigator.clipboard.writeText(command.value)
    ElMessage.success('已复制')
  } catch {
    // 非 HTTPS 或权限被拒时 clipboard 不可用。不报错——命令就在眼前，
    // 手动选中复制即可；一个红色错误会让人以为命令本身有问题
    ElMessage.info('剪贴板不可用，请手动选中复制')
  }
}

async function revoke(t: JoinTokenView) {
  try {
    await ElMessageBox.confirm(
      `吊销之后这张 token 立刻不能再用于加入。已经加入的节点不受影响。`,
      '确认吊销',
      { type: 'warning', confirmButtonText: '吊销' },
    )
  } catch {
    return
  }
  try {
    await api.post(`/nodes/tokens/${seg(t.id)}/revoke`)
    ElMessage.success('已吊销')
    await tokens.reload()
  } catch (e) {
    ElMessage.error(e instanceof ApiError ? e.message : String(e))
  }
}

/** 三种失效是三件不同的事，现场最先要知道的正是「是哪一种」 */
function tokenState(t: JoinTokenView) {
  if (t.usable) return { text: '可用', type: 'success' as const }
  return { text: t.reason || '不可用', type: 'info' as const }
}
</script>

<template>
  <AppShell>
    <h1 class="text-xl font-semibold">加入节点</h1>

    <el-alert type="info" :closable="false" class="mt-4" show-icon>
      <template #title>这里只生成命令，不代你登录目标机器</template>
      <div class="text-sm">
        让 mechd 去 SSH 意味着它要持有能登录所有节点的私钥，而 M7 的一条主要收益
        正是把长期暴露面从「SSH 常开」降为零入站端口。命令请在**目标机器**上执行。
      </div>
    </el-alert>

    <el-card class="mt-4" shadow="never">
      <div class="mb-3 font-medium">生成加入凭据</div>
      <div class="flex flex-wrap items-end gap-4">
        <div>
          <div class="mb-1 text-sm text-gray-500">绑定节点名</div>
          <el-input v-model="node" class="w-48" placeholder="留空 = 不绑定" />
        </div>
        <div>
          <div class="mb-1 text-sm text-gray-500">有效期</div>
          <el-input v-model="ttl" class="w-28" placeholder="30m" />
        </div>
        <div>
          <div class="mb-1 text-sm text-gray-500">可用次数</div>
          <el-input-number v-model="uses" :min="1" :max="100" class="w-32" controls-position="right" />
        </div>
        <el-button type="primary" :loading="creating" @click="create">生成</el-button>
      </div>

      <!--
        绑名与不绑名的区别要说在生成之前，不是之后。
        一张不绑名、可多次使用的 token 拿到手上就能往集群里塞机器。
      -->
      <div class="mt-3 text-sm text-gray-600">
        <template v-if="node">
          绑定了节点名的 token <b>只能</b>签出 CN={{ node }} 的证书——冒名进不来。
        </template>
        <template v-else-if="uses > 1">
          <span class="text-amber-700">
            不绑名 + 可多次使用：这是给批量预置（cloud-init / 镜像）留的口子。
            拿到它的人可以往集群里塞一台机器，因此有效期请尽量短。
          </span>
        </template>
        <template v-else>不绑名的单次凭据：加入时由那台机器自报名字。</template>
      </div>
    </el-card>

    <el-alert v-if="error" type="error" :closable="false" class="mt-4" :title="error" />

    <el-card v-if="tokens.data.value" class="mt-4" shadow="never">
      <div class="mb-3 font-medium">已有凭据</div>
      <el-empty
        v-if="!tokens.data.value.tokens?.length"
        description="还没有加入凭据"
        :image-size="60"
      />
      <el-table v-else :data="tokens.data.value.tokens" row-key="id">
        <el-table-column label="绑定节点" min-width="140">
          <template #default="{ row }">
            <span v-if="row.node" class="font-mono">{{ row.node }}</span>
            <span v-else class="text-gray-400">未绑定</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="140">
          <template #default="{ row }">
            <el-tag :type="tokenState(row).type" size="small">{{ tokenState(row).text }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="用量" width="100">
          <template #default="{ row }">{{ row.used }} / {{ row.maxUses }}</template>
        </el-table-column>
        <el-table-column prop="expires" label="到期" min-width="180" />
        <el-table-column label="" width="90">
          <template #default="{ row }">
            <el-button v-if="row.usable" link type="danger" size="small" @click="revoke(row)">
              吊销
            </el-button>
          </template>
        </el-table-column>
      </el-table>
      <!--
        **明文不在这张表里**，因为它不在库里。一个「重新查看」的按钮
        要么是假的，要么意味着明文落了库——两者都不能有。
      -->
      <div class="mt-2 text-xs text-gray-400">
        凭据明文只在生成时显示一次（库里存的是哈希）。丢了就重新生成一张。
      </div>
    </el-card>

    <!-- 生成之后：唯一一次看到明文的机会 -->
    <el-dialog
      :model-value="fresh !== null"
      title="在目标机器上执行"
      width="720"
      @close="fresh = null"
    >
      <el-alert type="warning" :closable="false" show-icon class="mb-3">
        <template #title>这段内容只显示这一次</template>
        <div class="text-sm">关掉之后就拿不回来了——库里存的是哈希。</div>
      </el-alert>

      <el-alert v-if="localWarning" type="error" :closable="false" show-icon class="mb-3">
        <template #title>这个地址可能只有你能访问</template>
        <div class="text-sm">
          你正通过 <span class="font-mono">{{ origin }}</span> 访问。命令里的地址是
          <b>新节点要去连的</b>，请把它换成新节点能访问到的 mechd 地址。
        </div>
      </el-alert>

      <pre
        class="overflow-x-auto rounded bg-gray-50 p-3 font-mono text-xs dark:bg-[#262727]"
      >{{ command }}</pre>

      <div class="mt-2 text-xs text-gray-500">
        请确认 <span class="font-mono">{{ origin }}</span> 是新节点能访问到的地址。
        <code>--ca-hash</code> 是信任锚：目标机器用它校验 mechd 的身份，
        即便这条命令被人看到，他也伪造不出一个能通过校验的 mechd。
      </div>

      <template #footer>
        <el-button @click="fresh = null">关闭</el-button>
        <el-button type="primary" @click="copy">复制命令</el-button>
      </template>
    </el-dialog>
  </AppShell>
</template>

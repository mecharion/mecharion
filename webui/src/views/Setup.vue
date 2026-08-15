<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../lib/api'

// 首次访问的初始化页（ADR-0037）。
//
// **账号名固定显示 admin 且不可编辑**：这台机器只有一个管理员账号。
//
// **门禁靠初始化令牌，不靠 PoW/滑块**（ADR-0039）：初始化抢注是「谁先
// 提交谁赢」的一次性竞赛，PoW 只让「反复尝试」变贵，对第一次也是唯一
// 有意义的那次提交没有帮助——那套机制原本就没有接到服务端，接上也
// 验不动真正的风险。令牌是 mechd 首次启动时打印过的那个 admin token，
// 知道它就证明是刚装完这台机器的人。
const router = useRouter()
const password = ref('')
const confirm = ref('')
const token = ref('')
const submitting = ref(false)
const err = ref('')

async function submit() {
  err.value = ''
  if (password.value.length < 12) {
    err.value = '口令至少 12 个字符'
    return
  }
  if (password.value !== confirm.value) {
    err.value = '两次输入不一致'
    return
  }
  if (!token.value) {
    err.value = '请填写初始化令牌'
    return
  }
  submitting.value = true
  try {
    await api.post('/auth/bootstrap', { password: password.value, token: token.value })
    await router.replace('/login')
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="min-h-screen flex items-center justify-center bg-gray-50 px-4">
    <el-card class="w-full max-w-md" shadow="never">
      <h1 class="text-xl font-semibold">初始化 Mecharion</h1>
      <p class="mt-1 text-sm text-gray-500">
        这台机器还没有设过管理员口令。设定之后此页面不再出现。
      </p>

      <el-alert
        class="mt-4"
        type="warning"
        :closable="false"
        title="在你设定之前，任何知道初始化令牌的人都能完成初始化"
        description="请尽快完成。"
      />

      <el-form class="mt-5" label-position="top" @submit.prevent="submit">
        <el-form-item label="管理员账号">
          <el-input model-value="admin" disabled />
          <div class="mt-1 text-xs text-gray-400">
            这台机器只有一个管理员账号，名字固定为 admin。
          </div>
        </el-form-item>

        <el-form-item label="设定口令">
          <el-input v-model="password" type="password" show-password
                    placeholder="至少 12 个字符" />
        </el-form-item>
        <el-form-item label="再输一次">
          <el-input v-model="confirm" type="password" show-password />
        </el-form-item>

        <el-form-item label="初始化令牌">
          <el-input v-model="token" placeholder="mechd 首次启动时打印过的那个 admin token" />
          <div class="mt-1 text-xs text-gray-400">
            见启动这台机器时终端里的输出，或 &lt;配置目录&gt;/admin.token（0600，需要在这台机器上读取）。
          </div>
        </el-form-item>

        <el-alert v-if="err" type="error" :closable="false"
                  class="mb-3" :title="err" />

        <el-button type="primary" class="w-full" native-type="submit"
                   :loading="submitting">
          完成初始化
        </el-button>
      </el-form>
    </el-card>
  </div>
</template>

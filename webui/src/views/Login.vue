<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../lib/api'
import { useChallenge } from '../lib/useChallenge'
import SliderCaptcha from '../components/SliderCaptcha.vue'

const router = useRouter()
const ch = useChallenge()
const password = ref('')
const submitting = ref(false)
const err = ref('')

onMounted(ch.refresh)

async function submit() {
  err.value = ''
  const answer = ch.answer()
  if (!answer) {
    err.value = '验证还没算完，请稍候'
    return
  }
  submitting.value = true
  try {
    await api.post('/auth/login', {
      user: 'admin',
      password: password.value,
      challenge: answer,
    })
    await router.replace('/')
  } catch (e) {
    err.value = e instanceof Error ? e.message : String(e)
    // **每次失败都换一道新题**：题是一次性的，服务端已经把它核销掉了
    void ch.refresh()
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="min-h-screen flex items-center justify-center bg-gray-50 px-4">
    <el-card class="w-full max-w-md" shadow="never">
      <h1 class="text-xl font-semibold">登录 Mecharion</h1>

      <el-form class="mt-5" label-position="top" @submit.prevent="submit">
        <el-form-item label="账号">
          <el-input model-value="admin" disabled />
          <div class="mt-1 text-xs text-gray-400">
            这台机器只有一个管理员账号。
          </div>
        </el-form-item>

        <el-form-item label="口令">
          <el-input v-model="password" type="password" show-password
                    autofocus @keyup.enter="submit" />
        </el-form-item>

        <el-form-item label="人机验证">
          <SliderCaptcha
            v-if="ch.challenge.value"
            :background="ch.challenge.value.background"
            :piece="ch.challenge.value.piece"
            :piece-y="ch.challenge.value.pieceY"
            @change="(x: number) => (ch.sliderX.value = x)"
          />
          <div class="mt-2 w-full">
            <el-progress
              :percentage="ch.progress.value"
              :status="ch.progress.value === 100 ? 'success' : undefined"
            />
            <div class="text-xs text-gray-400">
              {{ ch.progress.value === 100 ? '验证已就绪' : '正在后台计算验证…' }}
            </div>
          </div>
        </el-form-item>

        <el-alert v-if="err || ch.error.value" type="error" :closable="false"
                  class="mb-3" :title="err || ch.error.value" />

        <el-button type="primary" class="w-full" native-type="submit"
                   :loading="submitting" :disabled="ch.progress.value !== 100">
          登录
        </el-button>
      </el-form>

      <p class="mt-4 text-xs text-gray-400">
        忘了口令？在服务器上执行 <code>mechctl user passwd</code> 重设。
      </p>
    </el-card>
  </div>
</template>

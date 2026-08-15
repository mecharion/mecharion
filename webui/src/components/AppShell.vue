<script setup lang="ts">
import { useRoute, useRouter } from 'vue-router'
import { api } from '../lib/api'

const route = useRoute()
const router = useRouter()

const nav = [
  { to: '/', label: '组件' },
  { to: '/deploy', label: '部署' },
  { to: '/nodes', label: '节点' },
  // 孤儿要有一个**固定的去处**：默认保留数据是 remove 的行为，而一个
  // 只能靠 CLI 才看得见的残留清单，等于没人会去看
  { to: '/orphans', label: '孤儿' },
]

async function logout() {
  await api.post('/auth/logout')
  await router.replace('/login')
}
</script>

<template>
  <div class="min-h-screen bg-gray-50">
    <header class="border-b bg-white">
      <div class="mx-auto flex max-w-6xl items-center gap-6 px-6 py-3">
        <router-link to="/" class="text-lg font-semibold no-underline text-gray-900">
          Mecharion
        </router-link>
        <nav class="flex gap-1">
          <router-link
            v-for="n in nav"
            :key="n.to"
            :to="n.to"
            class="rounded px-3 py-1.5 text-sm no-underline"
            :class="
              route.path === n.to
                ? 'bg-gray-100 text-gray-900 font-medium'
                : 'text-gray-500 hover:bg-gray-50'
            "
          >
            {{ n.label }}
          </router-link>
        </nav>
        <div class="ml-auto flex items-center gap-3 text-sm text-gray-500">
          <span>admin</span>
          <el-button link size="small" @click="logout">退出</el-button>
        </div>
      </div>
    </header>

    <main class="mx-auto max-w-6xl px-6 py-8">
      <slot />
    </main>
  </div>
</template>

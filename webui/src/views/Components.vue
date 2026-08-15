<script setup lang="ts">
import AppShell from '../components/AppShell.vue'
import { usePolled } from '../lib/useApi'
import type { ComponentView } from '../lib/api'

const { data, error, loading } = usePolled<{ components: ComponentView[] }>('/components')
</script>

<template>
  <AppShell>
    <h1 class="text-xl font-semibold">组件</h1>

    <el-alert v-if="error" type="error" :closable="false" class="mt-4" :title="error" />

    <el-card class="mt-4" shadow="never" v-loading="loading && !data">
      <el-empty v-if="data && !data.components?.length" description="还没有部署组件" />
      <el-table v-else :data="data?.components ?? []" row-key="name">
        <el-table-column label="名称" min-width="160">
          <template #default="{ row }">
            <router-link :to="`/components/${row.name}`" class="text-blue-600">
              {{ row.name }}
            </router-link>
            <!--
              **正在移除必须在列表里就看得见。** 它不接受任何写操作，
              而若与正常组件长得一样，用户点进去改配置只会撞上一句
              「不接受其它写操作」——那时他才知道发生了什么。
            -->
            <el-tag v-if="row.removing" type="danger" size="small" class="ml-2">
              正在移除
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column prop="pack" label="Pack" min-width="140" />
        <el-table-column prop="version" label="版本" width="120" />
        <el-table-column label="形态" width="120">
          <template #default="{ row }">
            {{ row.profile || '—' }}
          </template>
        </el-table-column>
        <el-table-column prop="instances" label="实例数" width="90" align="right" />
      </el-table>
    </el-card>
  </AppShell>
</template>

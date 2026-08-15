import { createRouter, createWebHistory } from 'vue-router'
import { api, type AuthState } from './lib/api'

// history 模式：地址栏干净。代价是服务端要把未知路径回退到 index.html——
// Go 侧的 handler 已经做了（只对没有扩展名的路径回退）。
export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', name: 'components', component: () => import('./views/Components.vue') },
    {
      path: '/components/:name',
      name: 'component',
      component: () => import('./views/ComponentDetail.vue'),
    },
    {
      path: '/components/:name/params',
      name: 'component-params',
      component: () => import('./views/ComponentParams.vue'),
    },
    { path: '/deploy', name: 'deploy', component: () => import('./views/Deploy.vue') },
    { path: '/nodes', name: 'nodes', component: () => import('./views/Nodes.vue') },
    { path: '/orphans', name: 'orphans', component: () => import('./views/Orphans.vue') },
    { path: '/join', name: 'join', component: () => import('./views/Join.vue') },
    { path: '/login', name: 'login', component: () => import('./views/Login.vue') },
    { path: '/setup', name: 'setup', component: () => import('./views/Setup.vue') },
  ],
})

// **未初始化时一律去初始化页，已初始化时不许再回去。**
//
// 这是前端的引导，不是防线：真正拦住重复初始化的是服务端那个一次性的
// 409（ADR-0037）。前端只负责别让用户看到一个走不通的页面。
router.beforeEach(async (to) => {
  if (to.name === 'login' || to.name === 'setup') {
    let state: AuthState
    try {
      state = await api.get<AuthState>('/auth/state')
    } catch {
      return true // 问不到就别挡路，让页面自己去报错
    }
    if (!state.initialized && to.name !== 'setup') return { name: 'setup' }
    if (state.initialized && to.name === 'setup') return { name: 'login' }
  }
  return true
})

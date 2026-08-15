import { createApp } from 'vue'
import 'virtual:uno.css'
import App from './App.vue'
import { router } from './router'

// **不 `.use(ElementPlus)`、不引全量 CSS。**
//
// 组件与样式由 unplugin-vue-components 的 ElementPlusResolver 按需引入
// （见 vite.config.ts）。全量注册会把整个组件库打进产物——而 mechd 装在
// 每一台机器上。
createApp(App).use(router).mount('#app')

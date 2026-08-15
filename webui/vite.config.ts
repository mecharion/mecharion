// 从 vitest/config 取 defineConfig，而不是 vite：
// 后者的 UserConfig 里没有 test 字段，vue-tsc 会当场报 TS2769。
import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'
import UnoCSS from 'unocss/vite'
import AutoImport from 'unplugin-auto-import/vite'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'

// 产物交给 `go generate`（internal/tools/webuibuild）搬运与压缩。
// 这里只负责构建，**不做压缩插件**——压缩用 Go 标准库做，
// 前端依赖树少一个是一个（ADR-0036）。
export default defineConfig({
  plugins: [
    vue(),
    UnoCSS(),
    // 按需引入：Element Plus 全量打包会明显变大，而 mechd 装在每台机器上
    AutoImport({ resolvers: [ElementPlusResolver()] }),
    Components({ resolvers: [ElementPlusResolver()] }),
  ],
  build: {
    // 指纹化文件名：配合 Go 侧的长缓存头（index.html 除外）
    assetsDir: 'assets',
    // 源码映射不进产物：它会让 embed 显著变大，而线上排障靠的是结构化日志
    sourcemap: false,
  },
  // 前端测试（23-web-ui §4.4.7）。
  //
  // 范围**只到纯逻辑与组件行为**，端到端是容器套件的活。加它的理由很具体：
  // 到第 6 步为止前端已经承载了三处「错了会毁掉用户输入」的逻辑
  // （只提交动过的字段、有改动时停轮询、unset 与空值分开），而它们此前
  // 只有 `vue-tsc` 这一道类型把关。
  test: {
    environment: 'happy-dom',
    include: ['src/**/*.test.ts'],
    // 每个测试文件一个干净的模块图：组合式函数里有模块级状态时，
    // 共享会让「单独跑绿、一起跑红」这类最难查的失败出现
    isolate: true,
    server: {
      // Vitest 默认把 node_modules 当外部依赖，交给 Node 原生 ESM 加载器
      // 处理，不经过 Vite 的转换管线——而 element-plus 按需引入时会带一条
      // 副作用 CSS import（`ElementPlusResolver` 默认行为），Node 不认识
      // `.css` 后缀，症状是 `Unknown file extension ".css"`。把它标为
      // inline 强制走 Vite 转换（Vite 会正常处理/剥离 CSS），不是绕过
      // 问题，是让它走该走的那条管线。D4 加组件测试时首次撞见——此前
      // 全部测试文件都是纯逻辑，不 mount 任何用到 Element Plus 组件的
      // .vue 文件，没人踩过这个坑。
      deps: { inline: ['element-plus'] },
    },
  },

  server: {
    // 开发期把 API 反代到本地 mechd，前端热更新不必重新构建 Go
    proxy: {
      '/api': { target: 'https://127.0.0.1:8443', changeOrigin: true, secure: false },
    },
  },
})

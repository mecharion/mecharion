/// <reference types="vite/client" />
// Vue + Vite + TS 的标准 shim：这份 `{}, {}, any` 是官方脚手架生成的
// 惯用写法（导入任意 .vue 文件时用得到的最宽松、安全的默认签名），
// 不是这个项目自己写的类型，不值得为它另找更精确的写法。
/* eslint-disable @typescript-eslint/no-empty-object-type, @typescript-eslint/no-explicit-any */
declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<{}, {}, any>
  export default component
}

// ESLint flat config（此前完全没有前端 lint）。
//
// 只接了通用规则集，**没有**加 eslint-plugin-vuejs-accessibility 之类的
// 无障碍规则——无障碍支持已经因为一个真实的设计冲突（真正的非视觉
// 替代方案等价于暴露 SliderCaptcha 的目标值，见 docs/design/23-web-ui.md
// §6.12.1）明确暂缓，这里不重复引入相关规则。

import js from '@eslint/js'
import tseslint from 'typescript-eslint'
import pluginVue from 'eslint-plugin-vue'
import globals from 'globals'

export default tseslint.config(
  {
    // 生成物与产物：unplugin-auto-import/unplugin-vue-components 写的
    // .d.ts 声明文件、构建产物、覆盖率报告都不是手写代码，lint 它们
    // 只会报一堆没有意义的问题。
    ignores: ['dist/**', 'coverage/**', 'auto-imports.d.ts', 'components.d.ts'],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  ...pluginVue.configs['flat/essential'],
  {
    languageOptions: {
      // 这是浏览器代码，不声明的话 window/navigator/HTMLElement 这类
      // 运行时环境自带的全局标识符会被当成"未定义的变量"误报。
      globals: globals.browser,
    },
  },
  {
    files: ['**/*.vue'],
    languageOptions: {
      parserOptions: {
        parser: tseslint.parser,
      },
    },
  },
  {
    rules: {
      // <script setup> 里模板用到的绑定，规则本身不认识编译宏产生的
      // 引用关系，全量开启会在完全正确的代码上报一堆假阳性。
      'vue/no-unused-properties': 'off',
      // 这个项目大量使用「先声明后台数据结构、暂时留空实现」的写法
      // （B-series 里多处"还没做的"标注同一个原则），未使用的函数参数
      // 常常是接口形状的一部分，不代表真的没用到。
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
    },
  },
  {
    // vue-router 挂载的路由页面，从不以自定义标签的形式出现在任何
    // <template> 里——这条规则要防的是"单词组件名与未来的原生/第三方
    // HTML 元素撞名"，对只会被 router 挂载、不会被 <tag> 引用的页面
    // 组件没有意义。
    files: ['src/views/**/*.vue'],
    rules: {
      'vue/multi-word-component-names': 'off',
    },
  },
)

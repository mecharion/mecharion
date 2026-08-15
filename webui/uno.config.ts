import { defineConfig, presetUno, presetIcons } from 'unocss'

// Tailwind 语法的原子类。布局与间距全用它，几乎不写 CSS 文件。
export default defineConfig({
  presets: [presetUno(), presetIcons({ scale: 1.2 })],
})

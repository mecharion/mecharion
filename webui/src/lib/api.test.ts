import { describe, expect, it } from 'vitest'
import { apiQuery, seg } from './api'

// CLI/API/UI 里拼 URL path/query 曾经是裸字符串拼接，没有统一
// 转义。这里验证的是转义本身的正确性——集成层面的验证（真实请求打到真实
// 后端、名字带特殊字符时不会被误路由）在 Go 侧的
// internal/cli/ctlcmd/apipath_integration_test.go 里，前端没有等价的
// 起真实 mechd 服务端的测试设施，因此这里只覆盖 seg/apiQuery 本身的
// 转义正确性，不重复起服务端。

describe('seg', () => {
  it('转义会打乱路径分段的字符，且能转义回原值', () => {
    const cases = ['a/b', 'a b', 'a?b=c', 'a#frag', 'a&b', '中文名字', 'a%2Fb', '']
    for (const name of cases) {
      const esc = seg(name)
      expect(decodeURIComponent(esc)).toBe(name)
      expect(esc).not.toContain('/')
    }
  })

  it('数字 id 也能转义（Join token 的 id 是 number）', () => {
    expect(seg(42)).toBe('42')
  })
})

describe('apiQuery', () => {
  it('编码特殊字符，省略空值', () => {
    const q = apiQuery({ role: 'a&b=c', site: '', node: 'n1' })
    const v = new URLSearchParams(q.slice(1))
    expect(v.get('role')).toBe('a&b=c')
    expect(v.has('site')).toBe(false)
    expect(v.get('node')).toBe('n1')
  })

  it('全部为空时返回空串', () => {
    expect(apiQuery({ a: '', b: undefined })).toBe('')
    expect(apiQuery({})).toBe('')
  })
})

import { describe, expect, it } from 'vitest'
import { fillJoinHost, isLocalOrigin } from './joinCommand'

// 验收表第 18 条要的是「**可复制的完整**命令」。一条带着 `<mechd-host>`
// 的命令粘过去就是一个 DNS 错误，而它看起来完全正常。

const raw =
  'mechlet install --join https://<mechd-host>:8443 \\\n' +
  '    --token m7n_join_abc123 \\\n    --ca-hash sha256:deadbeef'

describe('fillJoinHost', () => {
  it('把占位符换成浏览器连着的地址', () => {
    const out = fillJoinHost(raw, 'https://10.0.0.1:8443')
    expect(out).toContain('--join https://10.0.0.1:8443')
    expect(out).not.toContain('<mechd-host>')
  })

  it('token 与 CA 指纹一个字节都不动', () => {
    // 它们是凭据与信任锚。任何「顺手规范化一下」的处理都可能毁掉它们，
    // 而症状是加入时报「token 无效」或「指纹不匹配」——两条都指向别处
    const out = fillJoinHost(raw, 'https://10.0.0.1:8443')
    expect(out).toContain('--token m7n_join_abc123')
    expect(out).toContain('--ca-hash sha256:deadbeef')
  })

  it('端口跟着浏览器走，不是写死的 8443', () => {
    // 反向代理后面 mechd 可能在 443 上对外
    const out = fillJoinHost(raw, 'https://mechd.example.com')
    expect(out).toContain('--join https://mechd.example.com ')
  })

  it('origin 末尾的斜杠不会留在命令里', () => {
    const out = fillJoinHost(raw, 'https://10.0.0.1:8443/')
    expect(out).toContain('--join https://10.0.0.1:8443 ')
    expect(out).not.toContain('8443/ ')
  })

  it('没有占位符时原样返回', () => {
    // 将来服务端若改成填好的地址，这里不该再动它
    const filled = 'mechlet install --join https://real:8443 --token x'
    expect(fillJoinHost(filled, 'https://other:9999')).toBe(filled)
  })

  it('空命令给空串，不给 "undefined"', () => {
    expect(fillJoinHost('', 'https://x')).toBe('')
  })
})

describe('isLocalOrigin', () => {
  it('认出只有本机能访问的地址', () => {
    // 通过 SSH 隧道或本机打开 UI 时，生成的命令新节点连不上——
    // 那时界面要多说一句
    for (const o of [
      'https://localhost:8443',
      'https://127.0.0.1:8443',
      'http://localhost',
      'https://mechd.localhost:8443',
    ]) {
      expect(isLocalOrigin(o), o).toBe(true)
    }
  })

  it('真实地址不误报', () => {
    for (const o of [
      'https://10.0.0.1:8443',
      'https://mechd.example.com',
      'https://192.168.1.5:8443',
    ]) {
      expect(isLocalOrigin(o), o).toBe(false)
    }
  })

  it('不是合法 URL 时不报错', () => {
    expect(isLocalOrigin('这不是地址')).toBe(false)
  })
})

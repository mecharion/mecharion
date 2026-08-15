import { describe, expect, it } from 'vitest'
import { useParamEdits } from './useParamEdits'
import type { FormParam } from './api'

// 这三条规则每一条错了都会毁掉用户的输入，而在第 7 步之前它们只有
// `vue-tsc` 这一道类型把关（23-web-ui §4.4.7）。

function param(over: Partial<FormParam> = {}): FormParam {
  return { name: 'p', type: 'string', source: 'default', ...over }
}

describe('useParamEdits', () => {
  it('只提交动过的字段', () => {
    const e = useParamEdits()
    e.setValue('a', 1)

    // **判据是「没动过的不在里面」**，不是「动过的在里面」。
    // 后者在一个送全量的实现上照样成立——而送全量会把别人刚改过的
    // 另一个参数悄悄冲回旧值。
    expect(e.body(false).set).toEqual({ a: 1 })
    expect(Object.keys(e.body(false).set)).not.toContain('b')
  })

  it('撤销一次修改不等于把它改成空', () => {
    const e = useParamEdits()
    e.setValue('a', 'x')
    e.setValue('a', undefined)

    // undefined 是「撤销」，字段应当整个消失；若留下 a: undefined，
    // JSON 序列化后它变成 null——而 null 在服务端是一个**合法取值**
    expect(e.body(false).set).toEqual({})
    expect('a' in e.body(false).set).toBe(false)
  })

  it('把值改成空串是一次真实的修改', () => {
    const e = useParamEdits()
    e.setValue('a', '')
    // 空串是合法取值，不能被当成「没改」
    expect(e.body(false).set).toEqual({ a: '' })
    expect(e.dirty.value).toBe(true)
  })

  it('恢复默认与设值互斥', () => {
    const e = useParamEdits()

    e.setValue('a', 5)
    e.toggleUnset('a')
    expect(e.body(false).set).toEqual({})
    expect(e.body(false).unset).toEqual(['a'])

    e.setValue('a', 6)
    expect(e.body(false).unset).toEqual([])
    expect(e.body(false).set).toEqual({ a: 6 })
  })

  it('toggleUnset 可以取消', () => {
    const e = useParamEdits()
    e.toggleUnset('a')
    expect(e.isUnset('a')).toBe(true)
    e.toggleUnset('a')
    expect(e.isUnset('a')).toBe(false)
    expect(e.dirty.value).toBe(false)
  })

  it('改动计数把两种动作都算上', () => {
    const e = useParamEdits()
    expect(e.dirty.value).toBe(false)
    e.setValue('a', 1)
    e.toggleUnset('b')
    expect(e.changeCount.value).toBe(2)
    e.reset()
    expect(e.changeCount.value).toBe(0)
    expect(e.dirty.value).toBe(false)
  })

  it('请求体带上坐标，空坐标不出现在 body 里', () => {
    const e = useParamEdits()
    e.setValue('a', 1)

    const withGroup = e.body(true, { role: 'main', group: 'g1' })
    expect(withGroup).toMatchObject({ role: 'main', group: 'g1', dryRun: true })

    // 空字符串必须变成 undefined：`group: ""` 会被服务端当成
    // 「指定了组但名字是空的」，而不是「没指定组」
    const roleOnly = e.body(false, { role: 'main', group: '' })
    expect(roleOnly.group).toBeUndefined()
  })

  it('body 返回的是副本，改它不会污染内部状态', () => {
    const e = useParamEdits()
    e.setValue('a', 1)
    const b = e.body(false)
    b.set.injected = 'x'
    b.unset.push('sneaky')
    expect(e.body(false).set).toEqual({ a: 1 })
    expect(e.body(false).unset).toEqual([])
  })

  describe('canUnset', () => {
    it('本来就取默认值的参数不给「恢复默认」', () => {
      // 点下去什么也不会发生的按钮比没有按钮更糟
      expect(useParamEdits().canUnset(param({ source: 'default' }))).toBe(false)
    })

    it('被覆盖过的给', () => {
      const e = useParamEdits()
      expect(e.canUnset(param({ source: 'component' }))).toBe(true)
      expect(e.canUnset(param({ source: 'group' }))).toBe(true)
    })

    it('只读与不可变的不给', () => {
      const e = useParamEdits()
      expect(e.canUnset(param({ source: 'derived', readOnly: true }))).toBe(false)
      expect(e.canUnset(param({ source: 'component', immutable: true }))).toBe(false)
    })
  })
})

import { describe, expect, it } from 'vitest'
import { canPurge, isInstalled, since } from './orphans'

describe('孤儿的两个类别', () => {
  // 这条区分决定了界面上有没有那个「清理」按钮，因此判错的代价不对称：
  // 把「仍装着」当成「数据残留」，用户会点一下清理，然后以为问题解决了
  // ——而那个服务还在机器上跑着。
  it('有保留路径的是数据残留', () => {
    expect(isInstalled({ paths: ['/var/lib/mecharion/apps/web'] })).toBe(false)
  })

  it('没有路径的是「仍装着」，不能靠清理解决', () => {
    expect(isInstalled({ paths: [] })).toBe(true)
    expect(isInstalled({})).toBe(true)
    expect(isInstalled({ paths: undefined })).toBe(true)
  })

  it('只有数据残留才给清理按钮', () => {
    expect(canPurge({ paths: ['/var/lib/x'] })).toBe(true)
    // 仍装着 —— 清理停不掉它
    expect(canPurge({ paths: [] })).toBe(false)
    // 已经在等节点执行了，再点一次没有意义
    expect(canPurge({ paths: ['/var/lib/x'], purgeRequested: true })).toBe(false)
  })
})

describe('放了多久', () => {
  const now = Date.parse('2026-08-09T12:00:00Z')

  it('按量级换单位', () => {
    expect(since('2026-08-09T11:30:00Z', now)).toBe('30m')
    expect(since('2026-08-09T04:00:00Z', now)).toBe('8h')
    expect(since('2026-08-06T12:00:00Z', now)).toBe('3d')
  })

  // 刚上报的孤儿时间戳可能比本地时钟略新（两台机器的时钟不会完全一致）。
  // 那时不该显示成一个负数——它看起来像个 bug，而它只是时钟差。
  it('未来的时刻显示成 0m，不是负数', () => {
    expect(since('2026-08-09T12:00:30Z', now)).toBe('0m')
  })

  it('空值与畸形值不崩', () => {
    expect(since('', now)).toBe('-')
    expect(since('not-a-date', now)).toBe('-')
  })
})

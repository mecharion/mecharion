import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useLive } from './useLive'

// 判据集中在**降级**上：SSE 断了页面必须还是对的（23-web-ui §4.5.6）。
// 「实时」是一个优化，不是一个依赖。

class FakeEventSource {
  static last: FakeEventSource | null = null
  onerror: (() => void) | null = null
  closed = false
  private listeners: Record<string, ((ev: MessageEvent) => void)[]> = {}

  constructor(
    public url: string,
    public init?: EventSourceInit,
  ) {
    FakeEventSource.last = this
  }
  addEventListener(type: string, fn: (ev: MessageEvent) => void) {
    ;(this.listeners[type] ??= []).push(fn)
  }
  close() {
    this.closed = true
  }
  emit(type: string, data: string) {
    for (const fn of this.listeners[type] ?? []) fn({ data } as MessageEvent)
  }
  fail() {
    this.onerror?.()
  }
}

beforeEach(() => {
  FakeEventSource.last = null
  vi.stubGlobal('EventSource', FakeEventSource)
})

describe('useLive', () => {
  it('带上 cookie——EventSource 不能设请求头', () => {
    const l = useLive('/components/web/watch')
    l.start()
    // 认证只能靠 cookie，因此 withCredentials 必须为真；
    // 漏了它的症状是 401，而 EventSource 的错误信息里看不出原因
    expect(FakeEventSource.last?.init?.withCredentials).toBe(true)
    expect(FakeEventSource.last?.url).toBe('/api/v1/components/web/watch')
  })

  it('收到快照就整份替换，不做增量合并', () => {
    const seen: unknown[] = []
    const l = useLive<{ a: number; b?: number }>('/w', { onSnapshot: (s) => seen.push(s) })
    l.start()

    FakeEventSource.last!.emit('snapshot', JSON.stringify({ a: 1, b: 2 }))
    FakeEventSource.last!.emit('snapshot', JSON.stringify({ a: 9 }))

    // 第二条没有 b。**整份替换**意味着 b 消失，而不是留着上一条的 2。
    // 留着它就是增量合并，而那要求每条消息都不能丢。
    expect(l.data.value).toEqual({ a: 9 })
    expect(seen).toHaveLength(2)
  })

  it('第一条快照把状态切成 live 并通知调用方', () => {
    const onUp = vi.fn()
    const l = useLive('/w', { onUp })
    l.start()
    expect(l.state.value).toBe('connecting')

    FakeEventSource.last!.emit('snapshot', '{}')
    expect(l.state.value).toBe('live')
    expect(onUp).toHaveBeenCalledOnce()

    // 后续快照不该反复通知——调用方会用它来停轮询，重复调没意义
    FakeEventSource.last!.emit('snapshot', '{}')
    expect(onUp).toHaveBeenCalledOnce()
  })

  it('断流时退回轮询，且**不关掉** EventSource', () => {
    const onDown = vi.fn()
    const l = useLive('/w', { onDown })
    l.start()
    FakeEventSource.last!.emit('snapshot', '{}')

    FakeEventSource.last!.fail()

    expect(l.state.value).toBe('polling')
    expect(onDown).toHaveBeenCalledOnce()
    // 关掉就等于扔掉浏览器自带的重连与退避，然后自己写一个更差的
    expect(FakeEventSource.last!.closed).toBe(false)
  })

  it('反复报错只通知一次降级', () => {
    const onDown = vi.fn()
    const l = useLive('/w', { onDown })
    l.start()
    FakeEventSource.last!.fail()
    FakeEventSource.last!.fail()
    FakeEventSource.last!.fail()
    expect(onDown).toHaveBeenCalledOnce()
  })

  it('重连成功后回到 live', () => {
    const onUp = vi.fn()
    const onDown = vi.fn()
    const l = useLive('/w', { onUp, onDown })
    l.start()
    FakeEventSource.last!.emit('snapshot', '{}')
    FakeEventSource.last!.fail()
    expect(l.state.value).toBe('polling')

    // EventSource 自己重连成功 → 又来一条快照
    FakeEventSource.last!.emit('snapshot', '{"a":1}')
    expect(l.state.value).toBe('live')
    expect(onUp).toHaveBeenCalledTimes(2)
  })

  it('半条消息不弄坏手上那份', () => {
    const l = useLive<{ a: number }>('/w')
    l.start()
    FakeEventSource.last!.emit('snapshot', JSON.stringify({ a: 1 }))
    FakeEventSource.last!.emit('snapshot', '{"a":') // 截断

    // 解析失败时保留上一份完整状态：把它清空会让界面闪一下空白，
    // 而下一条快照几秒后才来
    expect(l.data.value).toEqual({ a: 1 })
    expect(l.state.value).toBe('live')
  })

  it('start 幂等，不会开出两条流', () => {
    const l = useLive('/w')
    l.start()
    const first = FakeEventSource.last
    l.start()
    expect(FakeEventSource.last).toBe(first)
  })

  it('stop 关掉流并回到 connecting', () => {
    const l = useLive('/w')
    l.start()
    const src = FakeEventSource.last!
    l.stop()
    expect(src.closed).toBe(true)
    expect(l.state.value).toBe('connecting')
  })
})

import { onUnmounted, ref, shallowRef } from 'vue'

/**
 * 连接状态。
 *
 * `polling` 不是「坏了」，是**降级但正确**：SSE 被反向代理吃掉时页面
 * 仍然对，只是慢一点（23-web-ui §4.5.6）。界面上要能区分这三种，
 * 但都不该报错——只有 `polling` 值得一个不显眼的提示。
 */
export type LiveState = 'connecting' | 'live' | 'polling'

/**
 * 订阅一条 SSE 流。
 *
 * **每条消息都是完整状态**（服务端保证，23-web-ui §4.5.1），因此这里的
 * 处理就是「用收到的覆盖手上的」——不需要按类型套用 patch，也不需要
 * 补发机制。断线重连之后的第一条就把状态修正了。
 *
 * `EventSource` 自带重连，我们不自己写重连循环：浏览器的实现考虑了
 * 退避与页面可见性，重写一遍只会更差。
 *
 * @param onSnapshot 收到快照时调用；由调用方决定怎么用
 * @param onDown     流断了时调用——调用方据此把轮询开回去
 */
export function useLive<T>(
  path: string,
  opts: { onSnapshot?: (snap: T) => void; onDown?: () => void; onUp?: () => void } = {},
) {
  const state = ref<LiveState>('connecting')
  const data = shallowRef<T | null>(null)
  const es = shallowRef<EventSource | null>(null)

  function start() {
    if (es.value) return
    // EventSource 不能设请求头，因此认证只能靠 cookie（同源自动带上）。
    // 这是浏览器 API 的硬限制，不是我们的选择。
    const src = new EventSource(`/api/v1${path}`, { withCredentials: true })
    es.value = src

    src.addEventListener('snapshot', (ev) => {
      try {
        data.value = JSON.parse((ev as MessageEvent).data) as T
      } catch {
        return // 半条消息：等下一条完整的，不要把手上那份弄坏
      }
      if (state.value !== 'live') {
        state.value = 'live'
        opts.onUp?.()
      }
      opts.onSnapshot?.(data.value as T)
    })

    src.onerror = () => {
      // **不 close()**：EventSource 会自己重连（服务端下发的 retry: 3000）。
      // 在这里关掉等于把浏览器那套退避逻辑扔了，然后自己写一个更差的。
      //
      // 但要把轮询开回去：重连可能永远不成功（反向代理不支持 SSE），
      // 而那时页面必须还是对的。
      if (state.value !== 'polling') {
        state.value = 'polling'
        opts.onDown?.()
      }
    }
  }

  function stop() {
    es.value?.close()
    es.value = null
    state.value = 'connecting'
  }

  onUnmounted(stop)
  return { state, data, start, stop }
}

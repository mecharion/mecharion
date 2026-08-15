import { onMounted, onUnmounted, ref, toValue, watch, type MaybeRefOrGetter } from 'vue'
import { useRouter } from 'vue-router'
import { api, ApiError } from './api'

/**
 * 拉一个只读接口，并按固定间隔刷新。
 *
 * **401 一律回登录页**：会话过期是很常见的事（12 小时），而一个停在
 * 「加载中」或者报「未授权」的页面对用户毫无用处。
 *
 * 轮询是第 4 步的做法；第 7 步会换成 SSE，轮询留作兜底。
 *
 * `path` 可以是响应式的：参数表单的路径带着 role / group 两个坐标，
 * 切换时必须重新拉。**换路径要立刻清掉旧数据**——否则切到 B 角色的
 * 瞬间屏幕上还是 A 的参数，而它们看起来完全像是 B 的。
 */
export function usePolled<T>(path: MaybeRefOrGetter<string>, intervalMs = 5000) {
  const router = useRouter()
  const data = ref<T | null>(null)
  const error = ref('')
  const loading = ref(true)
  let timer: number | undefined

  async function load() {
    // 记下发起时的路径：轮询与切换可能并发，晚到的旧响应不能覆盖新数据
    const at = toValue(path)
    try {
      const got = await api.get<T>(at)
      if (at !== toValue(path)) return
      data.value = got
      error.value = ''
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) {
        await router.replace('/login')
        return
      }
      if (at !== toValue(path)) return
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  watch(
    () => toValue(path),
    () => {
      data.value = null
      loading.value = true
      void load()
    },
  )

  /**
   * 暂停轮询。
   *
   * 有未保存改动的表单**必须**停掉轮询：否则下一次刷新会把用户正在填的
   * 值换成服务端那份，而那看起来像「输入框自己清空了」——这类现象没人
   * 会联想到轮询，只会当成界面有 bug。
   */
  function stop() {
    if (timer) {
      window.clearInterval(timer)
      timer = undefined
    }
  }

  function start() {
    if (timer === undefined) timer = window.setInterval(load, intervalMs)
  }

  onMounted(() => {
    void load()
    start()
  })
  onUnmounted(stop)

  return { data, error, loading, reload: load, stop, start }
}

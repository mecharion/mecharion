import { ref } from 'vue'
import { api, type Challenge } from './api'
import { solvePow, WasmUnavailableError } from './pow'

// 登录页用它防慢速撞库。
//
// **PoW 在后台自动算，滑块由用户拖**——两者并行：用户拖滑块的那几秒，
// 正好是 PoW 在算的时间，感知上几乎不额外等待。
//
// **初始化页（Setup.vue）不再用它**（ADR-0039）：初始化抢注是「谁先
// 提交谁赢」的一次性竞赛，PoW 只让「反复尝试」变贵，对唯一有意义的
// 那次提交没有帮助——那里换成了一次性 admin token 门禁。
export function useChallenge() {
  const challenge = ref<Challenge | null>(null)
  const powAnswer = ref<number | null>(null)
  const sliderX = ref(0)
  const progress = ref(0)
  const solving = ref(false)
  const error = ref('')

  async function refresh() {
    error.value = ''
    powAnswer.value = null
    sliderX.value = 0
    progress.value = 0
    try {
      challenge.value = await api.get<Challenge>('/auth/challenge')
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
      return
    }
    void solve()
  }

  async function solve() {
    const c = challenge.value
    if (!c) return
    solving.value = true
    try {
      powAnswer.value = await solvePow(
        {
          salt: c.powSalt,
          target: c.powTarget,
          difficulty: c.powDifficulty,
          memory: c.powMemory,
          time: c.powTime,
        },
        (done, total) => {
          progress.value = Math.round((done / total) * 100)
        },
      )
      progress.value = 100
    } catch (e) {
      if (e instanceof WasmUnavailableError) {
        // **换浏览器，或者干脆不用浏览器**：这台机器仍然可以完全用
        // mechctl（admin token）管理，不依赖这条走不通的登录路径——
        // 「换个浏览器再来」对运维现场未必可行，给一条真的能走的路。
        error.value = e.message + '。换一个浏览器，或改用 mechctl 命令行（用 admin token）管理这台机器。'
      } else {
        error.value = e instanceof Error ? e.message : String(e)
      }
    } finally {
      solving.value = false
    }
  }

  /** answer 返回可以提交的答案；还没算完时返回 null。 */
  function answer() {
    if (!challenge.value || powAnswer.value === null) return null
    return { id: challenge.value.id, pow: powAnswer.value, sliderX: sliderX.value }
  }

  return { challenge, powAnswer, sliderX, progress, solving, error, refresh, answer }
}

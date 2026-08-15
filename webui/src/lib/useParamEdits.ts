import { computed, ref } from 'vue'
import type { FormParam } from './api'

/**
 * 表单的编辑状态。
 *
 * 从 `ComponentParams.vue` 里抽出来，理由是**可测性**：这里的三条规则
 * 每一条错了都会毁掉用户的输入，而它们在 SFC 里只有类型检查这一道把关
 * （23-web-ui §4.4.7）。
 *
 * 三条规则：
 *
 *	① `edits` 里**只放动过的**。服务端是 PATCH（只合并提到的），前端若
 *	   送全量，「别人刚改过的另一个参数」会被这次保存悄悄冲回旧值
 *	② `unset` 与「改成空」是两个动作。一个参数的合法取值可以**就是**空串
 *	③ 两者互斥：一个字段要么设新值，要么恢复默认，不能同时
 */
export function useParamEdits() {
  /** 参数名 → 新值。**只装动过的**。 */
  const edits = ref<Record<string, unknown>>({})
  /** 要恢复默认的参数名。 */
  const unset = ref<string[]>([])

  /**
   * 设一个值。
   *
   * `undefined` 表示「撤销这次修改」——把字段清回服务端那份，而不是
   * 「把它改成空」。两者在 UI 上都表现为输入框空着，因此必须由调用方
   * 区分：secret 的空输入框意思是「不改口令」，string 的空输入框意思是
   * 「改成空串」。
   */
  function setValue(name: string, value: unknown) {
    if (value === undefined) {
      delete edits.value[name]
      return
    }
    edits.value[name] = value
    // 设了值就不能同时要求恢复默认
    const i = unset.value.indexOf(name)
    if (i >= 0) unset.value.splice(i, 1)
  }

  /** 切换「恢复默认」。与设值互斥。 */
  function toggleUnset(name: string) {
    const i = unset.value.indexOf(name)
    if (i >= 0) {
      unset.value.splice(i, 1)
      return
    }
    unset.value.push(name)
    delete edits.value[name]
  }

  function isUnset(name: string) {
    return unset.value.includes(name)
  }

  function isEdited(name: string) {
    return edits.value[name] !== undefined
  }

  const changeCount = computed(() => Object.keys(edits.value).length + unset.value.length)
  const dirty = computed(() => changeCount.value > 0)

  /** 组装请求体。 */
  function body(dryRun: boolean, coords: { role?: string; group?: string } = {}) {
    return {
      set: { ...edits.value },
      unset: [...unset.value],
      role: coords.role || undefined,
      group: coords.group || undefined,
      dryRun,
    }
  }

  function reset() {
    edits.value = {}
    unset.value = []
  }

  /**
   * 「恢复默认」这个按钮该不该出现。
   *
   * 只对**被覆盖过的**参数有意义：对一个本来就取 Pack 默认值的参数显示
   * 它，点下去什么也不会发生。只读与不可变的字段一律不给。
   */
  function canUnset(p: FormParam) {
    return !p.readOnly && !p.immutable && p.source !== 'default'
  }

  return {
    edits,
    unset,
    setValue,
    toggleUnset,
    isUnset,
    isEdited,
    changeCount,
    dirty,
    body,
    reset,
    canUnset,
  }
}

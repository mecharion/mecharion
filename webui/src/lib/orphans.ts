import type { OrphanEntry } from './api'

/**
 * 孤儿分两类，处置完全不同：
 *
 *   有路径  `remove` 留下的**数据残留** —— 清理就是删那几个目录
 *   无路径  下发里没有它了，但机器上**还装着、可能还在跑**
 *
 * 后者清理解决不了：它只删目录，停不掉进程。判错的方向很要命——
 * 把「仍装着」当成「数据残留」，界面会给出一个清理按钮，而用户点完
 * 之后会以为问题解决了，实际那个服务还在跑。
 *
 * 判据是「有没有保留路径」而不是别的：只有 remove 走完的实例才会留下
 * 收据，而收据里记的正是那些路径。
 */
export function isInstalled(o: Pick<OrphanEntry, 'paths'>): boolean {
  return !o.paths || o.paths.length === 0
}

/** 能不能对它执行清理。 */
export function canPurge(o: Pick<OrphanEntry, 'paths' | 'purgeRequested'>): boolean {
  return !isInstalled(o) && !o.purgeRequested
}

/**
 * 把一个时刻显示成「放了多久」。
 *
 * 绝对时间戳要人自己做减法，而这一列要回答的正是「它在那儿放多久了」
 * ——那个数字才决定要不要清。
 */
export function since(iso: string, now: number = Date.now()): string {
  const t = new Date(iso).getTime()
  if (!iso || Number.isNaN(t)) return '-'
  const ms = Math.max(0, now - t)
  const h = Math.floor(ms / 3_600_000)
  if (h < 1) return `${Math.floor(ms / 60_000)}m`
  if (h < 24) return `${h}h`
  return `${Math.floor(h / 24)}d`
}

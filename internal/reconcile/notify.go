package reconcile

import "sort"

// notify 的两个内建动作。其余取值是 hook 名。
const (
	NotifyRestart = "restart"
	NotifyReload  = "reload"
)

// notifySet 收集本轮调和产生的 notify 动作。
//
// 四条规则（06-state-and-drift §6.1）：
//
//	收集去重，调和结束后统一执行   三个 template 都 notify: reload，只 reload 一次
//	restart 吸收 reload            同一轮里既有 restart 又有 reload，只执行 restart
//	Diff 为空不触发                见 add 的注释
//	notify 失败算调和失败          配置改了但服务没重载，等于变更没生效
type notifySet struct {
	actions map[string]bool
	// causes 记录每个动作是被哪些资源触发的，只用于诊断输出。
	causes map[string][]string
}

func newNotifySet() *notifySet {
	return &notifySet{actions: map[string]bool{}, causes: map[string][]string{}}
}

// add 登记一个资源触发的 notify。
//
// **只应在该资源确实有差异并被 Apply 之后调用。**
//
// 调和每 60 秒跑一次，若无差异也触发 notify，服务会每 60 秒被重启一次，
// 永远无法稳定运行。而且这不是「配置一下就能避免」的——`apply` 与周期性
// 调和走同一条代码路径，引擎无法区分「用户主动触发」与「定时器到期」。
//
// 「我改了外部依赖，想强制重启」是显式意图，走 `mechctl component restart`。
func (n *notifySet) add(action, resourceID string) {
	if action == "" {
		return
	}
	n.actions[action] = true
	n.causes[action] = append(n.causes[action], resourceID)
}

// Empty 报告本轮有没有要执行的动作。
func (n *notifySet) Empty() bool { return len(n.actions) == 0 }

// resolved 返回去重且吸收后的动作列表，顺序稳定。
//
// restart 吸收 reload：既然进程要整个重来，再让它先热加载一次是纯粹的浪费，
// 而且那一次 reload 读的是即将被丢弃的进程状态。
func (n *notifySet) resolved() []string {
	if len(n.actions) == 0 {
		return nil
	}
	out := make([]string, 0, len(n.actions))
	restart := n.actions[NotifyRestart]

	for a := range n.actions {
		if a == NotifyReload && restart {
			continue
		}
		out = append(out, a)
	}
	sort.Strings(out)

	// restart 排到最前：hook 类的 notify 多半是「配置生效后做点什么」，
	// 在进程重启之后执行才有意义。
	for i, a := range out {
		if a == NotifyRestart {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return out
}

// absorbed 返回被 restart 吸收掉的动作，用于诊断输出。
func (n *notifySet) absorbed() []string {
	if n.actions[NotifyRestart] && n.actions[NotifyReload] {
		return []string{NotifyReload}
	}
	return nil
}

// causesOf 返回触发某个动作的资源 id。
func (n *notifySet) causesOf(action string) []string {
	c := append([]string(nil), n.causes[action]...)
	sort.Strings(c)
	return c
}

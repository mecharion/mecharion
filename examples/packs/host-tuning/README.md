# host-tuning — 无载荷、无进程的纯主机配置

**这个 Pack 没有 `blobs`，没有 `workload`，没有 `health`。** 它只声明主机应当处于什么状态。

它验证了一条从一开始就写在设计原则里、但此前从未被真正检验的判断：

> **主机配置与组件部署是同一个引擎。没有 `workload` 的 Pack 就是纯主机配置。**
> —— [06-state-and-drift §3](../../../docs/design/06-state-and-drift.md#主机配置与组件部署是同一个引擎)

## 覆盖的调优项

大数据 / 数据库组件安装前的常见前置：

| 类别 | 项 |
|---|---|
| 内核参数 | `vm.swappiness` `vm.max_map_count` `vm.overcommit_memory` `net.core.somaxconn` `fs.file-max` |
| 资源限额 | `nofile` `nproc` |
| SELinux | `/etc/selinux/config` 持久化 + `setenforce 0` 运行期 |
| 透明大页 | 开机 oneshot 单元（THP 设置不持久化） |
| swap | `swapoff -a` + 注释 fstab（**默认关闭**，破坏性） |

## 发现

### ① `systemd_unit` 资源在规范中缺失 ✅ 已补

关闭 THP 需要每次开机执行一次——它不是任何角色的「工作负载」，但确实需要一个 systemd 单元。

设计文档的资源清单里列了 `systemd_unit`，**但规范 §14 从未包含它**。写这个 Pack 时才发现这个遗漏。

补入后，两者的分野也随之写清：

| | `workload.runtime: systemd` | `systemd_unit` 资源 |
|---|---|---|
| 语义 | 这个角色的**受监管进程** | 一个 unit **应当存在并处于某状态** |
| 参与 | 健康检查、generation 切换、Rollout 编排 | 只是一条资源，参与漂移检测 |
| 典型 | 长驻服务 | `Type=oneshot` 开机任务、target、path 单元 |

**不要用 `workload` 表达 oneshot 任务**——`workload` 的健康检查、重启策略、Rollout 语义对它全都不适用。

### ② profiles 不只用于集群拓扑

此前所有 profile 都在表达「几个节点、哪些角色」。这里它是**调优预设**：

| profile | 用途 |
|---|---|
| `bigdata`（默认） | Hadoop / HBase / Kafka / Elasticsearch |
| `database` | PostgreSQL / MySQL（`overcommit_memory=2`，`max_map_count` 调低） |
| `minimal` | 受限或合规环境——不动 SELinux 与 THP |

profile 只覆盖了 `params.default`，一个角色都没改。**这说明 profile 的定义（一组角色 + 数量 + 放置 + 参数默认）确实与「拓扑」无关**，它只是「一组具名的预设」。

规范无需改动——但这个用法值得在文档里作为一等用例提及，否则用户只会想到集群形态。

### ③ 「持久化 + 运行期」是主机配置的固有二元性

SELinux 与 THP 都需要**两步**：

```yaml
- template: { dest: /etc/selinux/config, … }     # 持久化，重启后生效
- command:  { run: "setenforce 0", unless: … }   # 运行期立即生效
```

漏掉任何一步都是运维事故的常见来源（改了配置没重启 → 以为生效了）。

现有机制（`template` + 带守卫的 `command`）足以表达，**不需要新语法**。但这个模式会在几乎每个主机配置 Pack 里重复出现，值得在文档中给出惯用法。

## 验证了什么

| 规范条目 | 验证点 |
|---|---|
| 无 `blobs` 的 Pack | ✅ `platforms` 仍需显式声明——它表达「这些设置在哪些平台有效」 |
| [§14.4 主机配置资源](../../../docs/spec/pack-v1.md#144-主机配置) | ✅ `sysctl` `limits` 全覆盖，新增 `systemd_unit` |
| [§14.5 守卫](../../../docs/spec/pack-v1.md#145-逃生舱) | ✅ `setenforce` / `swapoff` / `sed` 三条命令各有 `unless` |
| [§14.1 `when`](../../../docs/spec/pack-v1.md#141-通用字段) | ✅ 每组危险操作都由布尔参数控制，可整块关闭 |
| [§13 profiles 作为预设](../../../docs/spec/pack-v1.md#13-profiles--部署形态) | ✅ 不改角色、只改参数默认值 |

## 与 ad-hoc 执行的取舍

同样的事也可以用一次性命令做：

```bash
mechctl node exec -l role=datanode -- sysctl -w vm.swappiness=1
```

两者的差别不在「能不能做到」，而在**做完之后**：

| | Pack | `mechctl node exec` |
|---|---|---|
| 记录期望状态 | ✅ | ❌ |
| 漂移检测 | ✅ 有人手工改回去会被发现 | ❌ |
| 新节点加入 | ✅ 自动应用 | ❌ 要记得再跑一遍 |
| 重装系统后 | ✅ 自动恢复 | ❌ 丢失 |
| 审计 | ✅ 谁在何时改了哪个参数 | ⚠️ 只记录命令，不记录意图 |

**判据：一次性排查用 `exec`，任何应当长期保持的状态都用 Pack。** 主机调优显然属于后者——它恰恰是最容易被人手工改掉又没人发现的东西。

这也是[原则四](../../../docs/design/00-overview.md#原则四声明式为主干命令式为逃生舱)（声明式为主干，命令式为逃生舱）在主机配置场景的具体体现。

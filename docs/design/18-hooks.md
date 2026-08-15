# hooks 执行

Pack 的逃生舱：引擎表达不了的一次性动作（`initdb`、建复制用户、格式化元数据）。

规范见 [spec §16](../spec/pack-v1.md#16-hooks)。本文只讲**执行语义**——
谁决定跑不跑、怎么拿密钥、失败了怎么办。

---

## 1. 职责切分

```
mechd    决定「这个 hook 这次要不要下发」   ← scope / when 在此求值
mechlet  收到就执行                        ← 完全不理解 once 语义
```

`when: false` 的不下发；`scope: once` 且已执行过的不下发
（[12-spec-and-state §1.4](12-spec-and-state.md)）。

**为什么 `once` 的仲裁在 mechd**：它是一个**跨节点**语义——「整个 Component
只执行一次」需要知道其它节点上发生了什么。放在唯一有全局视角的地方，
mechlet 就永远不需要相互查询，这是整个系统能够简单的关键。

`scope: once` 落在该角色 **ordinal 最小的实例**上。由于 ordinal
[一次分配后固化](../adr/0028-stable-ordinals.md)，这个选择在扩缩容后**不会漂移**——
否则一次扩容就可能让 `initdb` 在另一台机器上再跑一次。

## 2. 生命周期点

```
preInstall  → 资源阶段 → postInstall
preUpgrade  → generation 切换 → postUpgrade
preStop  → Runtime.Stop  → postStop
preStart → Runtime.Start → postStart
```

一次首装走 `preInstall` → 资源 → `postInstall` → `preStart` → 启动 → `postStart`；
一次升级把 `Install` 换成 `Upgrade`，并在中间夹上停止与切换。

**调和器知道自己在做哪一种**（generation 是否切换、是否首次物化），
因此 hook 点的选择不需要额外信息。

> **`postInstall` 在启动之前。** 需要服务已经在跑的动作——连上数据库建角色、
> 建库、灌初始数据——必须声明在 `postStart`。
>
> 这不是理论问题：`examples/packs/postgresql` 的 `bootstrap-roles.sh` 原本
> 就声明在 `postInstall`，而它要 `psql` 连上去执行 SQL。那样会在一个还没
> 起来的库上重试到超时。**判据：脚本要不要连自己这个服务？要就是 `postStart`。**

### 2.1 `notify` 指向 hook

除生命周期点外，`notify: <hook 名>` 让**资源变更**触发一个 hook
（[spec §14.1](../spec/pack-v1.md#141-通用字段)）：某个模板变了才跑，
与「安装到了哪一步」无关。

它走同一个执行器，因此密钥注入、脱敏、超时、身份的规则完全一致。
名字对不上任何已下发的 hook 时**明确报错**——静默跳过会让 Pack 作者
以为它生效了，而问题要到生产上才暴露。

## 3. 执行环境

| 项 | 值 |
|---|---|
| 身份 | `user`（默认 root） |
| cwd | generation 目录 |
| 环境变量 | `MECHARION_COMPONENT` `_ROLE` `_PROFILE` `_GENERATION` `_ORDINAL` `_PATHS_*` `_PARAM_*` |
| **敏感参数** | **不进环境变量**，走 `MECHARION_PARAM_FILE_<NAME>` 指向的 `0600` 临时文件 |
| 超时 | hook 级 `timeout`，缺省取 `hooks.timeout` |
| 输出 | stdout/stderr 收进事件流，**按 sensitive 参数值脱敏** |

临时文件落在 `/run/mecharion/hooks/<随机>/`（tmpfs），
**无论成败都在 hook 结束后立即整目录删除**。

> 环境变量会出现在 `/proc/<pid>/environ` 与崩溃转储里，且被子进程继承。
> 这是把口令交给一个任意脚本时最容易漏掉的泄漏面。

### 3.1 非 root 的 hook 怎么读到 0600 的文件

以 `user: postgres` 运行的 hook 读不了 root 拥有的 `0600` 文件。
**放宽权限位是错的解法**——那等于让同机任何用户都读得到。正确做法是两步：

| 层 | 权限 | 为什么 |
|---|---|---|
| `/run/mecharion/hooks` | `0711` root | **可穿过但不可列举**。少了 `+x`，下面做什么都白搭；给了 `+r` 则等于公开「这台机器上有哪些组件在跑什么」 |
| `<随机>/` 与其中的文件 | `0700` / `0600`，**属主改为 hook 的运行身份** | 真正的保护在这一层 |

**`+x` 必须逐级检查，不能只看最后一级。** 叶子目录是刚建出来的、必然合规；
真正拦住人的往往是上面某个早先被建成 `0700` 的中间层。那时的现象是
「文件的权限与属主都对，就是读不到」，排查时极容易一直盯着叶子看。

> 曾出现过 `/run/mecharion` 由更早一次运行建成 `0700`，而修正逻辑从叶子
> 开始判断、当场就满足了「已经可穿过」而退出的情形。

### 3.2 超时必须杀掉整棵进程树

脚本 fork 出的子进程会继承 stdout 的管道写端。只杀那个脚本的话，子进程
仍握着管道，`Wait` 就一直阻塞在读端上——**一个 `sleep 30 &` 足以让 300 秒的
timeout 形同虚设**，而现象是「超时了但函数没返回」。

因此命令跑在自己的进程组里，超时时杀整组；再用一层 `WaitDelay` 兜底，
防止有进程 `setsid` 逃出进程组后仍然吊住 `Wait`。

被 `SIGKILL` 掉的进程报的退出码是 `-1`。**超时要先于退出码判断**，
否则用户只会看到一句「退出码 -1」——既没说清发生了什么，也没告诉他该怎么办。

## 4. 失败处理

| 点 | 失败后 |
|---|---|
| `preInstall` / `preUpgrade` | **中止**，不物化 / 不切换。升级场景保持在旧 generation |
| `postInstall` / `postUpgrade` | 标记调和失败并上报；**已切换的不自动切回**（M6 的 Rollout 才做回滚决策） |
| `preStop` | 中止停止流程 |
| `postStart` | 标记失败；服务已经起来了，不自动停掉 |

**hook 不做重试。** 一个不幂等的 hook 重试一次就可能把事情做坏两遍，
而引擎无从判断它是否幂等。重试是运维的显式决定（重新 apply）。

## 5. 幂等是 Pack 作者的责任

`scope: once` 只保证「引擎不会重复下发」，不保证「脚本本身幂等」。
两者的区别在异常路径上会暴露：hook 执行到一半 mechlet 崩溃，重启后
mechd 并不知道它做到哪一步了。

因此**执行标记在 hook 成功之后才写**，而崩溃重启会导致重跑。示例 Pack 的
写法应当示范这一点：

```sh
if EXISTS (SELECT FROM pg_roles WHERE rolname = '${APP_USER}') THEN
    ALTER ROLE ... WITH PASSWORD '...'      -- 已存在就改，不报错
ELSE
    CREATE ROLE ...
END IF;
```

这也顺带让**轮换**能复用同一个 hook——重跑一次就把数据库里的口令改成新的
（[16-secrets §5](16-secrets.md#5-轮换必须产生新-generation)）。

## 6. hermetic

hooks 受 `mechpack lint --hermetic` 的静态扫描约束（[spec §17](../spec/pack-v1.md#17-hermetic-规则)），
且测试容器刻意不装 curl / wget / 包管理器索引——**测试环境本身成为约束的执行者**，
比静态扫描更彻底（[test/README](../../test/README.md)）。

## 7. 相关

- [spec §16 hooks](../spec/pack-v1.md#16-hooks)
- [ADR-0028 ordinal 固化](../adr/0028-stable-ordinals.md)（`once` 的落点稳定性）
- [16-secrets §6](16-secrets.md#6-hook-的密钥注入)

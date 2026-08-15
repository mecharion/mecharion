# 解析管线

把「Pack + 用户输入 + 节点事实」变成每个 RoleInstance 的
[ResolvedSpec](12-spec-and-state.md)。

这是 mechd 里最长的一段逻辑，也是最需要**顺序正确**的一段——每一步都依赖
前一步的产出，而依赖关系不是显然的。

---

## 1. 全景

```
① 选定 Pack 与 profile
② 放置 → RoleInstance 列表 + ordinal          （15-placement）
③ 解析参数
     Pack 默认 → Component → Role → ConfigGroup
     ↓
     defaultFrom 求值    需要：Node.Facts
     generate 求值       需要：SecretVault（仅首次）
     ↓
④ 解析依赖绑定 → 求值 provider 的 exports      需要：provider 已走完 ①–③
⑤ from 求值                                   需要：topology + exports
⑥ 渲染 paths → 渲染 templates → 生成 resources
⑦ 计算 notify 的最终动作
⑧ 封装：secretRef 替换、Seal(digest)
```

## 2. 参数解析链

```
Pack 默认值  →  Component 级  →  Role 级  →  ConfigGroup 级
```

**不存在第五层**（[ADR-0021](../adr/0021-config-group.md)）。单节点差异表现为
一个只含该节点的组——允许无名的 per-node 覆盖，就是允许配置雪花化。

同一个参数名在多层出现时后者覆盖前者。**类型与约束只在 Pack 里声明一次**，
每层的取值都按它校验；某层给了个非法值就在那一层报错，不留到渲染时才炸。

## 3. 三个「计算出来的值」的求值时机

`defaultFrom` / `generate` / `from` 依赖的东西不同，因此**不能同时求值**：

| 字段 | 依赖 | 时机 | 用户可覆盖 |
|---|---|---|---|
| `defaultFrom` | `Node.Facts` | 参数链解析后、逐实例 | ✅ 覆盖后不再求值 |
| `generate` | SecretVault | 同上，且**仅首次** | ✅ 用户给了值就不生成 |
| `from` | topology、requires.exports | 依赖绑定之后 | ❌ 完全推导 |

### `defaultFrom` 是逐实例的

```yaml
heap:
  defaultFrom: '{{ min (div .Node.Facts.Memory.Total 2) "8GB" }}'
  default: 1GB
```

**同一个 Component 的不同实例可以得到不同的值**——它们在不同的机器上。
这也是它必须在放置之后求值的原因。

求值失败（事实缺失、除零）回落到 `default` 并记录告警，**不中止部署**：
一个采集不到内存的节点不该阻断整个 Rollout（[spec §7.4](../spec/pack-v1.md#74-from-与-defaultfrom)）。

### `generate` 只在首次

```
参数已有值（用户给的 / 已生成过的）  →  原样用
否则                                →  生成一次，写入 SecretVault，固化
```

**每轮调和都重新生成会让密码每 60 秒换一次**，服务永远连不上。固化的键是
`(component, param)`，与实例无关——同一个 Component 的所有实例共用一份凭据。

## 4. 依赖绑定与 exports

```
requires.packs[].name  →  Site 内满足版本约束的 Component
```

绑定规则（[spec §5.2](../spec/pack-v1.md#52-绑定规则)）：恰好 1 个自动绑定，
多个则拒绝并要求 `--require zookeeper=zk-hdfs`，0 个则拒绝并列出缺什么。

**绑定一旦确定就固化**，之后不再重新解析——否则新装一套 ZooKeeper 就可能让
已有部署静默改指向。与[路径固化](04-paths-and-storage.md)、
[ordinal 固化](../adr/0028-stable-ordinals.md)是同一条原则的第三次应用。

绑定确定后求值 provider 的 `exports`：

```yaml
# provider
exports:
  app:
    role: primary
    fields:
      host:     "{{ .Address }}"
      password: "{{ .Params.app_password }}"   # ← secret 参数
```

产出 `RequireBinding.Exports`，其中每个字段带一个**推导出来的**敏感标记：
字段引用的参数是 `secret` → 该字段敏感。

### 敏感传播

> 消费方的参数取值来自敏感字段时，**mechd 直接把该参数标为 sensitive**，
> 不管消费方 Pack 声明的是什么。

这条**不是** lint 能做的——lint 只看得见一个 Pack，而依赖方可能来自别处、
单独发布（[spec §5.4](../spec/pack-v1.md#54-exportsfields--具名字段与凭据)）。
放在这里既是唯一有全局视角的地方，也让它**不可能被遗忘**。

消费方没声明 `type: secret` 时提示建议补上，**但不阻断部署**——不能因为别人
Pack 的声明问题卡住你的部署。

反过来，消费方标了 `secret` 而取值不含任何敏感字段时也提示一句：
**过度标注同样有害**，它会让排障时连「连的哪个库」都看不到，标记本身退化成噪音。

## 5. 组件间的解析顺序

`{{ .Requires.postgresql.Exports.app.password }}` 要求 PG 已经走完 ①–③。
因此一次涉及多个 Component 的 apply 必须**按 requires 图拓扑排序**：

```
zookeeper → kafka
jdk11 → java-webapp
postgresql → java-webapp
```

环在 lint（R37）与放置阶段各查一次。排序结果同时被 Rollout 复用——
**解析顺序与下发顺序是同一个顺序**，不需要两套。

## 6. 渲染

模板引擎与函数集见 [spec §9.1](../spec/pack-v1.md#91-引擎)。三条约束：

- **受限函数集**，不提供 env / exec / 文件读取 / 网络——任何能绕过 hermetic 的都不给
- `missingkey=error`：引用未定义的键直接失败，不产出半份配置
- 渲染在 mechd，**mechlet 不做模板渲染**（[ADR-0006](../adr/0006-multi-role-pack.md)）

产出 `Resource.Args` 中已渲染的 `content`。`template` 资源的 `src` 在
ResolvedSpec 里**不应存在**——出现即 mechd 的 bug，mechlet 会报错。

### 唯一的 late-bound 值

`{{ .Paths.Generation }}` 留给 mechlet 做字面量替换——generation 序号是
mechlet 本地分配的，mechd 无从得知（[12-spec-and-state §1.4](12-spec-and-state.md)）。

## 7. notify 的最终动作

Pack 里 `template` 声明 `notify: reload`，但若这次变化的参数中有
`restartRequired: true` 的，最终动作应当是 `restart`。

**这个判断由 mechd 做**——它持有上一版 spec，知道哪些参数变了。结果直接写进
`Resources[i].Notify`。mechlet 只做聚合去重与 `restart` 吸收 `reload`，
不判断「该 restart 还是 reload」。

## 8. 封装

```
① 敏感值 → @@m7n:secret:<id>@@ 占位，值不进 spec   （16-secrets §4）
② secretRefs: [{id, version}]                      参与 digest
③ Seal(spec) → digest
```

## 8.1 参数在模板里的形态

`size` 与 `duration` 不是裸字符串，而是带访问器的值
（[spec §7.2](../spec/pack-v1.md#72-类型12-种)）：

```
{{ .Params.tick_time }}                → 2000ms
{{ .Params.tick_time.Milliseconds }}   → 2000
{{ .Params.max_body_size.Bytes }}      → 4000000
{{ div .Params.heap 2 }}               → 直接参与算术，不必先写 .Bytes
```

**同一个参数的形状必须与它走哪条路无关。** `defaultFrom` 里
`{{ min (div .Node.Facts.Memory.Total 2) "31GB" }}` 算出来的是字节数；
若原样留作 `"8589934592"`，而 `default` 是 `"2GB"`，同一个参数在模板里就有
两种形状，引用它的每个 Pack 都得自己处理这个差异。因此 `size` 的求值结果
在参数层归一回人读的形式（`FormatSize`），只取**整除**的单位——
`8.5GB` 这种带小数的写法在各家应用的解析器里行为不一。

落进已解析规格时只留字面量：规格是线格式，不需要这层渲染期的便利，
存成对象还会让 digest 依赖一个纯属实现细节的东西。

## 9. 可复现性

除 `generate` 首次生成与 ordinal 首次分配外，**管线是纯函数**：

```bash
mechctl component render -f plan.yaml     # 不碰任何机器、不连 mechd
```

`plan.yaml` 就是 mechd 从库里读出来的那些东西：站点、节点（含 facts 与
volumes）、放置结果、三层参数覆盖、依赖绑定。字段与 `render.Request`
一一对应，**刻意不做任何推断**——这条命令的价值在于「同样的输入必然得到
同样的输出」，一旦开始猜就不成立了。拼错的字段直接报错，不静默忽略。

离线模式下密钥用一次性值：规格里只有引用、没有明文，因此产出可以直接
传阅；代价是 digest 与生产环境不可比（版本号来自真实的 Vault）。
这条限制在输出里明说，不让人自己发现。

这不是调试便利，是事故复盘的第一手材料：「为什么这台机器上是这份配置」
必须能离线回答。同一条管线少走「落库 + 下发」两步就是 `--dry-run` 与 `diff`，
**不另写一套预演逻辑**——两套实现迟早会不一致，而不一致的预演比没有预演更糟。

## 10. 相关决策

- [ADR-0006 一个 Pack 可含多个 Role，渲染发生在放置之后](../adr/0006-multi-role-pack.md)
- [ADR-0021 ConfigGroup](../adr/0021-config-group.md)
- [ADR-0024 跨 Pack 引用](../adr/0024-cross-pack-reference.md)
- [ADR-0028 ordinal 固化](../adr/0028-stable-ordinals.md)

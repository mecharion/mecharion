# ADR-0032: `ExecIn` 进 Runtime 接口

- **状态**：已接受
- **日期**：2026-08-04
- **相关**：[ADR-0010](0010-runtime-abstraction.md)、[ADR-0011](0011-docker-compose-in-v1.md)

## 背景

[ADR-0010](0010-runtime-abstraction.md) 给 Runtime 接口定了三条划界规则，
第①条是：

> **健康检查不进接口。** `http` / `tcp` / `exec` 探针在任何 Runtime 下行为
> 完全一致，写一次即可。

M2 只有 systemd 时这条成立。M4 开始做 docker runtime 时它不成立了：

```yaml
health:
  exec: { command: ["pg_isready", "-p", "5432"] }
```

裸机上 `pg_isready` 在 `{{ .Paths.Current }}/bin` 下；容器化之后它**只存在
于容器镜像里**，宿主机上没有这个文件。同一份 Pack 换个 runtime，探针就
探不动了。

三种探针里只有 `exec` 有这个问题：`http` 与 `tcp` 打的是**已发布的端口**，
在宿主机上照常可达。

## 决策

给 Runtime 接口加一个原语：

```go
// ExecIn 在工作负载的上下文里执行一条命令。
ExecIn(ctx context.Context, ref Ref, cmd []string) (command.Result, error)
```

| Runtime | 实现 |
|---|---|
| systemd | 就在宿主机上执行——**工作负载的上下文就是这台机器** |
| docker | `docker exec <container> <cmd>` |
| compose | `docker compose exec <service> <cmd>` |

ADR-0010 的规则①改写为：

> **健康检查的编排不进接口，但「在哪里执行」进。**

探针的重试、阈值、超时、结果归一化仍然只写一次，`internal/health` 不知道
自己在跟谁打交道；只有 `exec` 探针把「在哪执行」这一步委托给 Runtime。

## 理由

### ① 这正是 ADR-0010 预言的那类错误

ADR-0010 的核心论点是「接缝正确性只有第二个实现才能验证」，而这就是一个
实例：**第一个实现下这个洞不可见**——systemd 的工作负载上下文恰好就是宿主机，
于是「在哪执行」这个问题根本不会被问出来。

这也正是 [ADR-0011](0011-docker-compose-in-v1.md) 把 docker 提前到
M5/M6 之前的收益：这次修正发生在漂移检测与升级编排**写出来之前**，
成本只是改一个接口和一处调用；等它们写完再改，代价高一个量级。

### ② 不是为健康检查开的口子

`mechctl component exec <target> -- <cmd>` 本来就在
[CLI 动词表](../design/10-cli.md#43-component)里。运维要在一个容器化组件上
执行一条诊断命令时，需要的是完全相同的原语。

**一个动词两个用途**，而不是为健康检查特开一条路。若只为探针加，
将来实现 `component exec` 时会发现要再加一个几乎一样的方法。

### ③ 边界仍然清晰

加进去的是「在哪执行」，不是「怎么判定健康」。后者的复杂度（startupGrace、
interval、failureThreshold、successThreshold、连续成功/失败的状态机）
全部留在 `internal/health` 里，三个 Runtime 一份实现。

判据：**如果一件事在不同 Runtime 下的答案不同，它属于接口；否则不属于。**
「`pg_isready` 在哪跑」的答案不同，「连续失败三次算不健康」的答案相同。

## 后果

### 收益

- `exec` 探针在容器化组件上可用，「健康检查跨 Runtime 行为一致」这条承诺
  真正成立
- `mechctl component exec` 有了现成的底座
- 接缝的修正发生在 M5 / M6 之前，改动面最小

### 代价

- **Runtime 接口多一个方法**，每个新 Runtime 都要实现它。这是真实的成本，
  但它小于「让 Pack 作者为不同 runtime 写不同的探针」
- **compose 的实现需要 service 名**。project 是不透明的工作负载
  （ADR-0011），而 `exec` 必须指到具体 service。v1 的做法：取 project 里
  的第一个 service，并允许 Pack 在 `workload.compose` 中声明
  `execService`。**若没声明且 project 有多个 service，明确报错**，
  不猜——猜错了会在一个无关的容器里跑诊断命令
- **超时与信号语义要对齐**。`docker exec` 的退出码是容器内命令的退出码，
  但 `docker exec` 本身也可能失败（容器没在跑）。两者必须区分开：
  前者是「探针失败」，后者是「探不了」

### 需要专门测试的点

- systemd 实现的行为与改造前**逐字节一致**（这一步不该改变任何现有行为）
- 容器没在跑时 `ExecIn` 报的是「探不了」而不是「探针失败」——
  前者不该被计入 `failureThreshold`

## 参考

- `docker exec` / `docker compose exec` 的退出码语义
- Kubernetes 的 `exec` probe（同样在容器内执行，同样与 httpGet/tcpSocket 分开处理）

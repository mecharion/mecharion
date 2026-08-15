# M10 执行日志

只追加,按时间顺序排列;已写下的条目不删除、不改写,有新认识就另起一条并说明推翻了什么。

## 2026-08-12

- 建立 `docs/dev/` 目录与本里程碑的记录结构,目的是把 M10 的执行过程(任务拆解、每次调整、真机验收踩的坑)与 `design/adr` 的最终结论分开,避免重演 REV-027 描述的"设计文档混入实施日记"。
- `plan.md` 的任务表由 [06-defect-register-and-roadmap.md](../../review/20260809/06-defect-register-and-roadmap.md) 整理而来,阶段 A–D 与验收标准照抄该文档 §5/§6;标 `*` 的行是本次整理时按 §4 缺陷表的领域描述补充归位,不是审阅原文逐条排定的顺序。
- 尚未开始任何一项 REV 的实际修复;下一步是逐项制定具体的开发计划(设计取舍、验收测试)并开始实施,从阶段 A(P0)开始。

## 2026-08-12（续）：A1 REV-001 标识符校验

**设计。** 复用 Pack 自己早已强制的 DNS label 规则（`mechpack lint` R02/R09），而不是发明新规则——理由与最终形态写进了 [design/09-naming-conventions.md §7](../../design/09-naming-conventions.md#7-运行期标识符component--node--site--configgroup)。关键决定：
- Component/Site/ConfigGroup 用单段 RFC 1123 label；Node 用点号分隔的 subdomain（默认取自 `os.Hostname()`，真实主机名常带 FQDN 点号，单段规则会拒掉大量合法主机名）。
- 唯一强制校验点选在 `internal/store` 的四个 repo 写入口，不是每个 CLI/HTTP 调用方各自校验——逐一确认过这是全仓库唯一能落库这四类实体的地方（含 `mechlet install --standalone` 直接调 repo、绕过 `mechd.Service` 的路径）。
- 数据库 CHECK 约束本轮**不做**：SQLite 加约束要整表重建，而 Go 层已是完备、无法绕过的强制点，代价与收益不成比例，列为已知欠账，代价写在设计文档里而不是藏起来。
- Role 名（来自 Pack 定义）不进 `internal/ident` 的校验范围——那是 Pack 信任边界的问题（ADR-0016），不是运维标识符的问题；改用通用的「结果必须落在预期目录内」兜底检查（`internal/pathutil`），只加在 `internal/agent/desired.go` 这个真正把 Role 拼进裸文件名的地方。

**实现。** 新增 `internal/ident`（校验）与 `internal/pathutil`（边界检查）两个包；接入 `internal/store/repo.go` 四处、`internal/agent/desired.go`（Component+Role 拼成的实例文件名）、`internal/pki/pki.go` `IssueNode`（作为三方共用原语自我校验，不信任调用方）、`internal/cli/mechletcmd/install.go`（默认节点名只转小写、不做进一步改写，仍不合法就报错并提示 `--node`）。

**变异验证踩的坑（记下来避免下次重犯）：** 第一版 `internal/agent` 的逃逸测试写错了——只用了 2 层 `"../"`，而 `pathFor` 把 `Component+"__"+Role` 直接当文件名，`Component` 前缀会让第一段变成 `"pg-main.."` 这种含 `..` 但不等于 `..` 的字面量，第一个真正的 `".."` 只会抵消这个字面量、人还在原地——测试断言的是「返回了 error」，而 mutation 之后测试竟然还是绿的，一查是 `os.WriteFile` 在一个不存在的目录上失败，跟我要防的东西完全无关，属于误报的绿。改成算清楚要几层 `"../"` 才真正跳出 `desired` 目录、落到一个**确实存在**的兄弟目录，再去核对文件有没有被写出来，这才是真正测到了东西。四处修复全部过了「删掉校验，测试必须变红」的变异验证（Site/Node/Component/ConfigGroup 在 `internal/store`，`internal/agent`、`internal/pki`、`internal/cli/mechletcmd` 各一处）。

**验证范围。** `go build/vet/test ./...` 在 Windows 宿主与 WSL（Linux）两侧都过。容器化 e2e 验收套件（`./hack/testenv.sh up && test`，M2–M9 全量，含 `mechlet install --standalone` 真实走一遍）第一次跑出一批 `token 无效` 失败——排查后发现是我自己操作造成的：之前一次交互式 `up`/`test` 被 timeout 打断，容器里留了个孤儿 mechd 进程占着 `0.0.0.0:8444`，新起的 mechd 打印的 token 其实没人在用。`testenv.sh down` 干净重来一遍（`up` → `test` 一条链不中断）后，除 `test-ctlcmd` 外全部 `PASS`，包括之前失败的那批 drift/e2e 测试。

`test-ctlcmd` 的两个失败（`TestDocumentedVerbsExistOrAreMarked`、`TestMarkedVerbsAreReallyMissing`）是 `docdrift_test.go` 用相对路径 `../../../docs/design/10-cli.md` 读文档——这条路径只有在 `go test` 以包目录为 cwd 运行时才对；`hack/testenv.sh` 的 `cmd_test` 把测试二进制放到容器里的 `/tmp/m7n-test` 下跑（读了源码确认的：`docker exec -w "$workdir" ...`），那里没有挂源码树，所以路径必然解析不到，跟传的内容无关。**这与本次改动无关**——没碰过 `ctlcmd` 或 `10-cli.md`，且该失败是路径结构性的，与 REV-001 的字符集/校验逻辑毫无关系，在任何一次提交上用同样的方式跑都会复现。记在这里但不算进 A1 的验收范围；这类「测试二进制离开 go test 语境后相对路径失效」的问题更适合作为 REV-032/CI 卫生的独立条目，不在本轮顺手扩大范围去修。

A1 到此完成：Go 层实现 + 双平台单元测试（含变异验证）+ 容器化 M2–M9 验收全绿。下一步进 A2（REV-002，Join 读后写竞态）。

## 2026-08-12（续）：A2 REV-002 Join 读后写竞态

**设计。** 读完 `internal/mechd/join.go` 现状：token 可用性检查、节点存在性检查、`Nodes().Upsert`（会静默覆盖同名节点）、`JoinTokens().Use`（无条件 `used+1`、失败仅 warning）是四次独立的读写，中间的窗口能被并发请求同时钻进去——两台物理机器最终都能拿到 CN 相同、被 CA 承认的有效证书，而吊销是按节点名查状态（ADR-0034），废不掉冒充的那一张。

关键决定：
- 新增 `Repos.ClaimNode(ctx, tokenID, n Node, now time.Time) (Node, error)`，在**同一个事务**里做「条件消耗 token（`used < max_uses AND revoked_at IS NULL AND expires_at > now`，用 `:execrows` 判断有没有真的抢到）+ 真正的 INSERT（不是 Upsert，撞 `UNIQUE(site_id, name)` 就返回 `ErrNodeTaken`）」。任一失败整体回滚——名字冲突不该额外消耗 token 的使用次数，否则一个专挑已占用名字抢注的攻击者能白白耗尽别人的合法 token。
- 证书签发仍在事务之外、先于这次调用（`Service.Join` 保持「先签发再落库」不变）：一张签发了但没用上的证书是无害孤儿，节点没落库，任何 RPC 都会在身份收口处被拒，也从没被返回给失败请求的调用方。
- `Service.Join` 里原有的存在性预检查**保留但降级为「快路径」**：只为常见非并发场景给一个友好、指名道姓的错误，本身不再承担并发安全的职责——过了不代表真的抢得到。
- `NodeRepo.Upsert` 不动：`mechlet install --standalone` 的本地幂等重装、`Service.AddNode` 仍然合理地要用 upsert 语义。这次只解决 Join 这一条路径——AddNode 自身残留的（低得多，需要已认证的管理员权限）竞态不在 REV-002 范围内，没有顺手扩大。
- 删掉了变成死代码的 `JoinTokenRepo.Use` 与 `UseJoinToken` SQL 查询（唯一调用方就是被替换掉的那段代码）。

**踩的坑：** `ClaimNode` 第一版直接读 `r.s.Now()`（`*store.Store` 自己的时钟），而 token 的 `ExpiresAt` 是 `Service.CreateJoinToken` 用 `Service.now()`（测试夹具里的假时钟）算出来的——两者在现有测试夹具里是两个独立配置的时钟，`store.Open` 从未把假时钟传给 `Store`。这是第一处 store 层代码真正拿时间做业务判断（而不是单纯盖 `CreatedAt` 戳），因此第一次暴露了这个潜在不一致：token 用假时钟的 2026-01-01 算出的过期时间，拿真实墙钟一比全部「已过期」。改成 `now` 由调用方显式传入（`Service.Join` 传 `s.now()`），不读 `r.s.Now()`——这也更符合 `SetRevoked(ctx, id, at *time.Time)` 这类既有方法的约定：需要调用方判断的时间点显式传，`r.s.Now()` 只用于不参与逻辑判断的记账戳。

**实现。** `internal/store/queries/expected.sql` 新增 `UseJoinTokenIfAvailable`（`:execrows`，带条件的 UPDATE）与 `InsertNode`（不带 `ON CONFLICT` 的纯 INSERT），`sqlc generate` 重新生成。`internal/store/repo.go` 新增 `ClaimNode`（唯一的安全边界）与 `isUniqueViolation`（靠 `modernc.org/sqlite` 的 `*sqlite.Error.Code() == SQLITE_CONSTRAINT_UNIQUE` 类型判断，不做字符串匹配）。`internal/mechd/join.go` 的 `Join()` 把三次独立调用换成一次 `ClaimNode`。

**变异验证：**
- store 层：把 `ClaimNode` 临时改回「先查再写、非原子、token 不设条件」的旧模式，`-race` 下并发测试立刻抓到——同名并发 50 次冒出 18 次成功（本该恰好 1），maxUses=5 却放行了全部 50 次（token 形同虚设）。恢复后全绿。
- mechd 层：把 `Service.Join` 的接线改回旧的 `Nodes().Upsert`，同样的两条并发测试立刻变红（4 次成功 / 30 次成功，均不符合预期），确认不是「测试凑巧测不到接线错误」。恢复后全绿。

**验证范围。** `go build/vet/test ./...`（Windows + WSL）全绿；`go test ./internal/store/... ./internal/mechd/... -race`（WSL，含全部既有测试，不只是新增的）全绿，没有数据竞争。

3 节点集群 e2e（`hack/testenv.sh cluster`，真实网络 mTLS 握手下的 Join）跑完整套 `test/multinode`：`TestNodeJoinsWithToken`、`TestRemoteNodeJoinsOverMTLS` 等全绿；`TestJoinRejectsBadTokens` 的 7 个子用例里，「已过期的 token」（1s TTL、sleep 2s 再 join）第一次跑输了——单独重跑（同一个还热着的集群，不重建）稳定通过。判定为负载敏感的计时抖动，不是 REV-002 的真 bug，理由三条：(1) `Usable()`/过期判断这两处我完全没有改动过；(2) 同一次全量里另外还炸了两个跟 Join 毫无关系的 rollout 批次门禁测试（`TestRolloutBatchesGateRealMachines`、`TestResumePicksUpWhereItStopped`），报的错正是决策清单 #330 早就记录在案的「多节点全量套件在长时间连续运行的机器上会出现负载敏感的失败」——同一类环境问题，不会因为这次改的是 Join 就单独绕开它；(3) 1s TTL 只留 2s 容错窗口本来就是这条测试自己选的紧余量，全量跑 40 分钟后系统更容易在这种窄窗口上被调度延迟顶到。没有把这当成"通过了就算了"含糊过去——单独重跑复现了"通过"，而不是随便找个理由开脱。

跑完后 `hack/testenv.sh cluster down` 清理。

## 2026-08-12（续）：A3 REV-003 Deploy 不是业务原子操作

**设计。** 读 `Service.Deploy` 现状：Component 落库、Instance ensure/delete、事实冻结、渲染（渲染管线内部 `resolveBindings`/`bindOne` 会顺手固化依赖绑定）是四次独立的写，任一步失败前面写完的部分不会退回——库里可能出现一个不带任何实例的 Component，或者放置都做完了却没能渲染出规格的悬空状态。

关键决定：**事务通过 `context.Context` 传递，不改渲染管线的函数签名**。`renderComponent`/`renderWith`/`renderWithGroups` 在 mechd 内部有 13 处调用点，绝大多数是预览/diff 路径,根本不关心事务；给每处加一个 `tx` 参数的代价和出错面都不成比例。改为 `Store.InTx(ctx, func(context.Context) error)`——回调拿到的是挂着事务的 ctx，`wq(ctx)`/`rq(ctx)` 认出它就用同一个连接，认不出就照旧各走各的，因此 Deploy 之外的调用方行为一字不变。完整设计写进了 [design/07-persistence.md §1.5](../../design/07-persistence.md#跨多个-repo-的事务走-context不改签名)。

`InTx` 做成**可安全嵌套**：ctx 里已经挂着事务时直接在那个事务里跑，不再开新的——`s.write` 只有一个连接，嵌套 `BeginTx` 会卡在等一个永远不会被外层放出来的连接上。这让 `InstanceRepo.Ensure`、A2 新增的 `Repos.ClaimNode` 不用改代码就能被 Deploy 这样更大的事务包住。

**踩的坑，而且是个真死锁。** 第一版把 Deploy 的 persistComponent→ensureInstances→freezeFacts→renderComponent 整段包进 `InTx` 后，`go test ./internal/...` 直接卡死，`-timeout 30s` 撞出 goroutine dump，精确指到 `vault.(*Vault).Generate` 卡在 `database/sql.(*DB).conn`。原因：`internal/vault` 与 mechd 主库共用同一个 SQLite（它直接 import 了 sqlcgen，本就不是完全隔离的两个包），此前把自己的 Querier 固定绑死在写连接池上（`sqlcgen.New(s.Writer())`，只在 `Open()` 时取一次）。Deploy 的事务持有唯一的写连接后，渲染管线生成新密钥去问 Vault，Vault 再去问连接池要一个新连接——池子只有一个，被 Deploy 自己攥着，谁都等不到谁。修法：给 `store.Store` 加一个面向包外的 `WriteQueries(ctx)`（`wq` 的导出版本），把 Vault 的 `q *sqlcgen.Queries` 固定字段换成按 ctx 现取——这也顺带让「生成了新密钥但事务回滚」的情形变得无害（孤儿密文，从未被任何规格引用，与 A2 的孤儿证书同一个道理）。

**实现。** `internal/store/store.go` 的 `InTx` 签名从 `func(*sql.Tx) error` 改为 `func(context.Context) error`，内部检测嵌套。`internal/store/repo.go` 新增 `WriteQueries(ctx)` 导出方法；`wq`/`rq` 改为接收 ctx、查 `txKey{}`。全包 80 处 `r.s.wq()`/`r.s.rq()` 调用点机械地加上 `ctx` 参数（跨 `repo.go`/`jointoken.go`/`batch.go`/`observed.go`/`session.go`/`user.go` 六个文件），`InstanceRepo.Ensure`、`Repos.ClaimNode` 改用新模式，不再手动 `sqlcgen.New(tx)`。`internal/vault/vault.go` 的 `Vault.q` 从固定字段改成按 ctx 现取的方法。`internal/mechd/service.go` 的 `Deploy` 把非 dry-run 分支的四步包进一个 `s.Store.InTx`。

**变异验证：**
- 把 Deploy 拆回非原子的分步提交，三条新增的原子性测试全部按预期变红，且失败信息精确复现了修复前的真实症状——`TestDeployRollsBackOnMissingDependency` 抓到「渲染失败后库里多了一个 Component」，`TestDeployRollsBackWhenInstanceEnsureFails` 抓到同样的悬空 Component，`TestDeployUpdateRollsBackLeavingOriginalStateIntact`（3→4 扩容中途失败）抓到「实例数从 3 变成了 4」。恢复后全绿。
- 死锁修复没有单独再做一次变异（复原再撞一次死锁成本较高），但已经在这次开发过程中被真实撞见、诊断、修复、复测四个阶段完整走过一遍，等同于经过了一次真实的「坏→好」验证。

新增测试用一个测试侧的 `failingRepos`（包一层 `store.Repos`，只在指定的第 N 次调用上注入错误，其余原样委托）做故障注入，不是生产代码的一部分。

**验证范围。** `go build/vet/test ./...`（Windows + WSL）全绿；`go test ./internal/mechd/... ./internal/store/... ./internal/vault/... -race`（WSL）全绿，无数据竞争，含 125s 的 mechd 全量（比平时慢是因为 `-race` 本身的开销，不是新问题）。容器化单节点 e2e（M2–M9 全量）跑完：21 个测试二进制全绿（含真实走一遍部署/漂移/回滚全流程的 test-e2e），唯一失败是 A1 时就确认过的 `test-ctlcmd` docdrift 路径问题（测试二进制脱离 `go test` 语境后相对路径失效，与本次改动无关，已知的独立欠账）。

A3 到此完成：Go 层实现 + 双平台 `-race` 单元测试（含变异验证）+ 容器化 M2–M9 验收全绿，过程中额外修了一个真实死锁（Vault 与主库共享唯一写连接）。下一步进 A4/A5（REV-004/005，HTTP 未认证入口的资源边界）。

## 2026-08-12（续）：A4/A5 REV-004/005 未认证入口的资源边界

两条放一起做是因为都是「未认证入口能不能被刷」这同一个问题的不同侧面，且都改 `internal/mechd` 的 HTTP 层，分开做只会来回切上下文。

**A4（REV-004）设计。** `decodeBody` 是全部 20 处 JSON 请求体解析的唯一入口，此前只做 `DisallowUnknownFields`，没有大小上限、没有独立的读取超时、不查 Content-Type、不挡请求体里的第二个 JSON 值。逐条补上：
- 体积：`http.MaxBytesReader` 上限 1 MiB（Pack 上传走自己的 4 GiB 上限，不受影响）。
- 读取时间：`http.ResponseController.SetReadDeadline`——**没有用 `http.Server.ReadTimeout`**，那是全局的，会连带卡住 Pack 上传合法的长时间大文件传输；`ResponseController` 是按单个请求设的，不影响其它路由。
- Content-Type：非空时必须是 `application/json`；不设不管（很多现有测试与脚本本来就不设它，不该因此被误伤）。
- 单值请求体：`Decode` 完主体后再 `Decode(&struct{}{})` 探一次，不是 `io.EOF` 就拒——标准库的 `json.Decoder` 本身只解析流里第一个 JSON 值，`{...}{...}` 这种体第二段会被静默丢弃。
- 状态码：`statusFor` 新增 `errors.As(err, &mbe)` 分支识别 `*http.MaxBytesError` 映到 413；`decodeBody` 其余的错误统一用 `faults.Permanentf`/`faults.Wrap` 显式标成 Permanent，不再指望字符串里凑巧含哪个关键词——`isUserError` 本来就是「显式标记优先，逐步加，不是一次性把所有错误点改一遍」，这次刚好补上 decodeBody 这一处。20 处调用点从硬编码 `http.StatusBadRequest` 统一换成 `statusFor(err)`（多行 perl 精确匹配 decodeBody 后面紧跟的那一行，没有误伤 `apply` 里另一处无关的 400）。

**A5（REV-005）设计。** `/auth/challenge` 不需要凭据，每次出题都要真算一次 Argon2、画两张滑块图。原来只有 `authn.Limiter.Check`——那是登录失败锁定，只问「这个来源有没有因为登录失败被锁定」，出题从不记录，不失败、只是拼命出题一样能把 CPU 吃满。新增 `authn.ChallengeLimiter`：单 IP + 全局两层滑动窗口（10/分钟、200/分钟），在真正出题**之前**查，两层缺一都不够——只挡 IP 挡不住很多个来源各自不超限、合起来超了的分布式流量。另外给 `authn.Store`（未核销挑战的内存表）加了 `MaxPendingChallenges`（2000）硬上限，独立于限流参数——万一以后限流被调宽，或者加了别的出题入口忘了接限流，这里仍然兜底。

**实现。** `internal/authn/challengelimit.go`（新文件）：`ChallengeLimiter`。`internal/authn/challenge.go`：`Store.Issue` 开头加硬上限检查（放在任何随机数生成/Argon2/画图之前，拒绝本身必须便宜）。`internal/mechd/authapi.go`：`challenge` handler 接入 `ChallengeLimiter.Allow`，`Issue` 返回 `ErrTooManyPending` 时映到 429。`internal/mechd/httpapi.go`：`API` 新增 `ChallengeLimiter` 字段，`Handler()` 里 nil 时兜底构造，与 `Limiter`/`Challenges` 同一个模式。

**变异验证：**
- A4：把 `decodeBody` 拆回只剩最基本的 `Decode`，5 条新测试里 4 条（体积、Content-Type、读取时间、单值）按预期变红，第 5 条（缺 Content-Type 时放行）是对照组，本该保持绿。
- A5：`ChallengeLimiter.Allow` 拿掉全部限制后，5 条新测试里 4 条变红（一条 per-IP 隔离测试没测到这条改动，符合预期）；`Store.Issue` 拿掉硬上限检查后专门的那条测试变红；HTTP 层接线测试（`TestChallengeIsRateLimited`）在 `challenge` handler 里去掉 `ChallengeLimiter.Allow` 调用后同样变红，且失败信息精确指出「Issue() 被多调用了一次」——证明测的是「真的没拦住」而不是巧合。

其中 `TestIssueRejectsWhenPendingCapReached` 第一版直接调用 2000 次真实 `Issue()`，跑到 20 秒——不是能接受的单测代价。改成直接往 `s.m`（同包可见）灌 2000 条轻量 dummy 记录，只保留「到了上限该不该拒」这一件要测的事，降到 0.00s。

**验证范围。** `go build/vet/test ./...`（Windows + WSL）全绿；`go test ./internal/mechd/... ./internal/authn/... -race`（WSL）全绿，无数据竞争。容器化单节点 e2e（M2–M9 全量，含 test-webui 真实走一遍登录/挑战流程）跑完：21 个测试二进制全绿，唯一失败仍是那个与本次改动无关的已知 `test-ctlcmd` docdrift 路径问题。

A4/A5 到此完成。阶段 A 还剩 A6（REV-006，首次初始化认证流程——需要新 ADR）、A7（REV-007，预登记节点 join）、A8（REV-008，HTTP 500 与错误分类）。

## 2026-08-12（续）：A6 REV-006 首次初始化认证流程——ADR-0039

**先写 ADR，再动代码**（用户明确要求这个顺序）。核心判断记在 [ADR-0039](../../adr/0039-bootstrap-token-gate.md)：`Setup.vue` 的 PoW/滑块从来没有接到服务端（`bootstrapAdmin` 只读 `password`），这是 REV-006 的字面发现；但往深想一层——**这套机制接上了也验不动真正的风险**。初始化抢注是「谁先提交谁赢」的一次性竞赛，PoW 只让「反复尝试」变贵，对第一次、也是唯一有意义的那次提交没有任何影响，属于套错了威胁模型（登录页的 PoW 是对的，那里防的正是反复尝试）。

候选过三个：(A) 把 PoW/滑块真接上——如上所述基本无效；(B) 限制 `/auth/bootstrap` 只认回环地址——与 ADR-0037 决策 193 的目标正面冲突（"要求先 SSH 敲命令等于把自动化链断在最后一步"，边缘设备的典型用法是装完之后拿另一台笔记本连过去初始化，那台笔记本不是回环）；(C) 复用 `mechd` 首次启动就已经生成、只打印一次的 admin token 作为 bootstrap 门禁。选 C——它不是发明新机制,是把两个"证明你是刚装完这台机器的人"的信号接到一起,操作者本来就在装机时看着那段输出,不需要为这一步单独再连一次。

**实现**：`BootstrapBody` 加 `token` 字段；`API` 加 `BootstrapTokenHash`（空值时无条件拒绝，不是"忘接线就悄悄放行"）；`serve.go` 把 `ensureToken` 算出来的 token 哈希后传入，顺手在启动横幅里加一句"这个 token 也是初始化令牌"；`Setup.vue` 撤掉 `SliderCaptcha`/`useChallenge`，换成一个令牌输入框（`Login.vue` 继续用 `useChallenge`，那里 PoW 是对的工具，未受影响）。

**校验顺序踩了一个坑，被真机测试当场抓到**：第一版是"先验令牌、再查是否已初始化"。`test/webui` 的 `Test01b_BootstrapIsOneShot`（真实走一遍"已初始化后再调 bootstrap 必须 409"）用的是不带 token 的请求——按第一版顺序会先被令牌校验拦成 401，而不是测试期望的 409。改成"先查是否已初始化（该判据对谁都是同一个答案，`GET /auth/state` 本来就无鉴权地公开着同一件事），再验令牌"。补了一条 `TestBootstrapAlreadyInitializedWinsOverBadToken` 精确钉住这个顺序，并且顺手发现 `test/webui/acceptance_linux_test.go` 里 `Test01_InitializeOnce`（真正完成一次首次初始化）原本的 POST 也没带 token——这条测试若不改会在真机验收里真的失败，不是本文档能读到的地方，是靠跑容器发现的，修了。

**变异验证**：把令牌校验那段拿掉，5 条新测试里 4 条按预期变红；恢复后确认顺序调整不影响其它既有的 8 条 bootstrap 相关测试（`user_test.go` 里全部走 `Service.InitializeAdmin` 直接调用，不经过 HTTP 层，本来就不受这次改动影响）。

**验证范围。** `go build/vet/test ./...`（Windows + WSL）全绿；`go test ./internal/mechd/... -race`（WSL）全绿。`webui` 侧 `vue-tsc -b && vite build` 与 `vitest run`（35 个既有前端测试）全绿。容器化单节点 e2e 第一轮跑的是改令牌顺序、改 test/webui 之前的旧二进制，及时停掉；干净重跑（新二进制、新容器）21 个测试二进制全绿，含真实带令牌完成一次首次初始化的 test-webui，唯一失败仍是那个与本次改动无关的已知 `test-ctlcmd` docdrift 路径问题。

A6 到此完成。阶段 A 还剩 A7（REV-007，预登记节点无法 join）、A8（REV-008，HTTP 500 泄漏与错误分类）。

## 2026-08-12（续）：A6 追加——`mechctl user bootstrap`（无人值守自动初始化）

用户看过 ADR-0039 后提出：应当再给 `mechctl` 加一条首次初始化命令，让完全无人值守的场景不需要人去找那个初始化令牌。这属于"继续把这份 ADR 做完"，不是新开一个决策，因此补进了 ADR-0039 本体（新增"CLI 侧的自动化入口"一节），没有另开 ADR-0040。

**设计**：`mechctl` 解析 admin token 早就有一条零配置读取链（`--token` flag → context 配置 → `/etc/mecharion/admin.token`，与 CA 证书零配置读取同一条纪律，见 08-security §3.2）。新命令 `mechctl user bootstrap [--password-file <path>]` 不引入任何新的信任来源，只是把 `Client` 已经解析出来的 token 值，同时当作 `POST /auth/bootstrap` 请求体里的 `token` 字段发出去——脚本在 `mechlet install --standalone` 之后紧接着跑这一条命令即可，全程不需要打开浏览器。远程执行时退回显式 `--token`，与 `mechctl context set` 一直以来的用法一致。

**实现**：`Client` 加 `Token()` 访问器（只给这一个调用方用，其余命令都不需要把 token 明文掏出来）；`user.go` 加 `newUserBootstrapCmd`，复用既有的 `readPassword` helper（`--password-file`，非交互环境不裸读 stdin）；`10-cli.md` §4.9 补充命令说明。

**测试**：3 条新测试用一个真实的 `httptest.Server` 桩加真实 cobra 命令执行路径（不是直接调函数），分别钉住"显式 --token 会被带上"、"零配置从 `DefaultTokenFile` 读到的 token 会被带上"（用 `MECHARION_ROOT` 环境变量重定向默认路径，与仓库里已有的 `TestMissingTokenIsExplained` 同一个既有模式）、"一个 token 都解析不出来时在本地就失败、不发任何请求"。全部做了变异验证（拿掉 `Token()` 调用，前两条按预期变红）。

**容器验证的取舍**：单元测试已经用真实的 cobra 执行路径 + 真实 HTTP 请求验证了这条命令本身的正确性；`/auth/bootstrap` 的服务端行为在 A6 前半段已经被容器验证过多次。尝试在容器里手动跑一遍 `mechlet install --standalone` 然后再跑 `mechctl user bootstrap` 时，撞上了一个与本次改动无关的环境问题——手动、脱离测试套件正常调用顺序地反复 `exec` 会让 `sync_bins` 与 `mechlet install` 自己的 generation/符号链接管理互相打架（`rename ... current.tmp ... current: file exists`），这是驱动方式的问题，不是产品的问题——自动化测试套件里同样的安装流程一直在正常工作。判断这个坑不值得为了"再验一遍已经充分验证过的东西"去继续排查，没有勉强凑一个看起来完整实际没测出新东西的"容器验证通过"。

**验证范围。** `go build/vet/test ./...`（Windows + WSL）全绿；`go test ./internal/cli/ctlcmd/... -race`（WSL）全绿。

## 2026-08-12（续）：A7 REV-007 预登记 Node 可以 Join

**设计。** [22-multi-node §6.15](../../design/22-multi-node.md#615-node-add-之后没法-join两条路互斥) 早就把问题问清楚了："已经通过 `mechd ca issue` 拿到证书、但还没连上来过的节点算不算 pending"——答案是不算，把它拆成两个状态值：

- `NodeReserved`（新）：`node add` 预登记，从未签发过证书。
- `NodePending`：已经拿到证书（join 或 standalone 安装），还没连上来过——语义比此前收紧了一格。
- `NodeSeen`：不变。

关键决定写进了 [22-multi-node §6.16](../../design/22-multi-node.md#616-补第三个状态值把-615-的问题接上)：

- 判据只有一个——`existing.Status == NodeReserved && !existing.Revoked()`。REV-002 要防的"同一身份两张有效证书"对 reserved 行不成立，因为认领之前根本没有第一张证书；被吊销的 reserved 行仍然拒绝（预登记之后又被吊销，不能靠 Join 绕过）。
- 不区分绑名/不绑名的 token——到达 `ClaimNode`时，`Service.Join` 已经用 `t.NodeName` 决定了最终要认领的名字，判定"这个名字能不能被认领"不需要再问一遍token 是怎么来的。这正是批量预置场景（预登记 N 个名字 + 一张不绑名、`max_uses=N` 的 token）需要的形态。
- 显示层不加第四个值：`nodeStatus()` 只看 `Seen()`，reserved 与"已发证书未连接"的 pending 对运维显示的都是同一句 `pending`——区分它们是 Join 内部的身份把关，不是给人看的状态。

`ClaimNode`（REV-002 的原子认领入口）从"名字不存在就 INSERT，否则报 `ErrNodeTaken`"扩成三路：不存在→INSERT；存在且 reserved 且未吊销→原地 UPDATE（`ClaimReservedNode` 新 SQL 查询，`WHERE status='reserved'`，认领与判定在同一个事务里，不用一条取巧的 `UPSERT...ON CONFLICT DO UPDATE WHERE` 换一个在 Go 侧更难分辨"是不是真认领成功了"的实现——`RETURNING` 在 WHERE 不匹配时仍然返回原行，会让"claimed"与"no-op 因为已经是 pending"两种情况在返回值里长得一样，这个坑在写实现前就想清楚了，没有掉进去）；存在但不是可认领的 reserved→`ErrNodeTaken`。SELECT 与后续写在同一个事务里，靠 SQLite 单写连接模型保证中间不会有别的写者插进来——与 `InstanceRepo.Ensure` 的"先查再写"是同一条既有纪律。

`Service.AddNode` 从写 `NodePending` 改成写 `NodeReserved`；`Service.Join` 的"存在性快路径"从无条件拒绝改成"存在且不是可认领的 reserved 才拒绝"，真正的把关仍在 `ClaimNode`。

**实现。** `internal/store/model.go`（新增 `NodeReserved` 常量与 `Node.Reserved()`）、`internal/store/queries/expected.sql` + `sqlc generate`（新增 `ClaimReservedNode`）、`internal/store/repo.go`（`ClaimNode` 三路分支）、`internal/mechd/service.go`（`AddNode` 改状态值）、`internal/mechd/join.go`（存在性快路径放行 reserved）、`internal/cli/ctlcmd/node.go`（`node add` 的文档注释更新，不再说"之后状态是 pending"）。

**变异验证。** 三处逐一验证：①`join.go` 的放行逻辑改回无条件拒绝——`TestJoinClaimsPreRegisteredNode` 按预期变红（"节点已在册"）；②`repo.go` 里"是否可认领"的整个判断改成恒为 `ErrNodeTaken`——`TestClaimNodeClaimsReservedNode`、`TestClaimNodeConcurrentReservedOnlyOneWins` 按预期变红；③单独去掉 `existing.Revoked()` 这一半判据——`TestClaimNodeRejectsRevokedReservedNode` 按预期变红。三处全部改回后复测为绿。

**测试。** `internal/store/claimnode_test.go` 新增 5 条（认领成功原地改行且保留 ID、拒绝被吊销的 reserved、拒绝已发证书的行、reserved 场景的 50-goroutine 并发只有一次成功）；`internal/mechd/join_test.go` 新增 2 条（`node add` 之后能被 Join 认领、被吊销之后不能）；`internal/store/claimnode_test.go`/`join_test.go` 里被 REV-007 改变了前提的既有测试（`TestClaimNodeRejectsTakenNameWithoutBurningToken` 用的是默认状态 `unknown`，`TestJoinRejectsAlreadyRegisteredName` 用的是 `f.addNode`——两者都不是 reserved，行为保持不变，只更新了注释说清楚"这条测的是已发证书的行，不是 REV-007 放行的那种"）。

**验证范围。** `go build/vet/test ./...` 在 Windows 与 WSL 两侧全绿；`CGO_ENABLED=1 go test ./... -race`（WSL）全绿。

**容器化验收踩的坑。** 第一次跑 `./hack/testenv.sh cluster test` 时新测试完全没有出现在输出里——查 `hack/testenv.sh` 才发现 `cluster_test` 只检查 `bin/test-multinode` 是否存在（`[ -x "$bin" ] || die ...`），**不会自己重新编译**；跑的是上一次 `hack/e2ebin.sh` 留下的旧测试二进制，里面根本没有本轮新加的两个测试函数。这与开发日志前面记过的"改了文件、没重启进程"是同一类坑的另一个变种：这次是"改了测试文件、没重新编译测试二进制"，宿主侧驱动程序同样不会自己发现源码变了。补跑 `./hack/e2ebin.sh` 重新交叉编译全部 `bin/test-*` 之后，同步新二进制到三个节点、重启 `mecharion-*` 服务，再跑一遍才是真的在测新代码。

**容器化验收结果（真实三节点集群，`m7n-n1/n2/n3`）：**
- `TestBatchPreRegisteredNodesJoinWithUnboundToken`（06 缺陷台账 §6 明确要求的"预置批量节点 E2E 成功"）——`PASS`（5.00s）：`node add` 批量登记 n2、n3 两个名字，共用一张不绑名、`--uses 2` 的 token，两台机器各自用 `--node <自己的名字>` 真实走 `mechlet install --join`（本机生成密钥、发 CSR、拿回证书），token 用量正确记为 2，两台随后都用领到的证书连上来并显示 `online`。
- `TestJoinRejectsRevokedPreRegisteredName`——`PASS`（2.05s）：预登记之后 `mechctl node revoke`，再拿绑名 token 去 Join，被拒绝并显示"已在册"。
- 同文件里 A2 遗留的 `TestNodeJoinsWithToken`、`TestJoinRejectsBadTokens`（7 个子用例）一并跑过，全部 `PASS`——确认这次改动没有破坏原有的 Join 校验链。

跑第一遍全量 `cluster test`（用的是那个未重建的旧二进制）时顺带发现两个**与本次改动无关**的既有失败：`TestRolloutBatchesGateRealMachines`、`TestResumePicksUpWhereItStopped`，都停在"第 3/3 批"没能在期限内收敛。没有改过 `internal/reconcile`/`rollout` 相关代码，且这两条测的是滚动升级门禁与真实机器的收敛时序，与 REV-007 的 Node 状态机不在同一个子系统——记在这里，不算进 A7 的验收范围，值得后续单独排查（可能是这次容器环境本身较慢、或是一个独立于本次改动的既有缺陷）。

## 2026-08-12（续）：A8 REV-008 HTTP 500 不泄漏内部错误

**设计。** 缺陷台账给的验收是三句话：typed error + 稳定 code + request ID；500 响应不含内部 error，日志保留完整 cause；改变中文消息不改变 HTTP status。三句话对应三个动作，写进了 [08-security §3.2「错误响应契约」](../../design/08-security.md#错误响应契约500-不泄漏内部信息分类不依赖文案)：

- **500 不回显 `err.Error()`。** `writeErr` 改成 `a.writeErr(w, r, code, err)`：非 500 的状态码（400/404/409/429...）文案原样返回——这些本来就是写给用户看的说明；500 时换成固定文案，真实 cause 连同 `requestId`/method/path 一起写进 `a.S.log().Error(...)`，不再出现在响应体里。
- **稳定 code。** 新增 `errCode(status int) string`，纯粹由 HTTP 状态码映射（`invalid_argument`/`not_found`/`conflict`/`too_large`/`rate_limited`/`internal`...），不看错误文案——程序化分支该读这个字段，不该匹配 `error` 里的中文。
- **request ID。** 新增 `withRequestID` 中间件包住 `Handler()` 返回的 `mux`：每个请求生成一个 8 字节随机 ID，回填到 `X-Request-Id` 响应头，也挂进 `context` 供 `writeErr` 取用、塞进响应体的 `requestId` 字段。**不信任客户端传入的同名头**——这台机器是唯一的控制面，没有上游服务需要跨进程传 trace id。
- **分类不再依赖中文文案。** 这是三条里工作量最大的一条：`isUserError` 原来「显式类型标记优先，退回中文子串匹配」的兜底逻辑，子串表（"放置校验失败""不在册""已存在""必须""不合法""没有声明""会移除""没有匹配""不存在""有多个候选"）删掉，只保留 `errors.As(err, &faults.Error) && Class == Permanent` 这一条判据。

  这条改动本身不难，难的是**删掉兜底之后不能有任何一个此前靠子串命中 400 的错误悄悄变成 500**——审阅时选了"全量转换"（而不是"先做 500/code/requestId，子串兜底留作已知欠账"）这个更彻底但工作量更大的选项：逐一核对 `internal/mechd`（`apply.go`/`applydoc.go`/`configgroup.go`/`form.go`/`orphans.go`/`release.go`/`removal.go`/`removecomponent.go`/`resolve.go`/`restart.go`/`rollout.go`/`rolloutpolicy.go`/`service.go`/`setparams.go`/`user.go`）与 `internal/placement`（`constraints.go`/`placement.go`）里全部 `fmt.Errorf`/`errors.New` 构造点（约 90 处），逐一判断"这是不是一个会流到 `statusFor(err)` 的、原本靠子串命中 400 的验证性错误"，是则显式包一层 `faults.Permanentf("", ...)`（约 65 处）。

  **明确排除在外、不打标记的几类**：
  - `internal/mechd/backend.go` 整个文件——那是 gRPC `Register` 处理器，不走 HTTP 的 `statusFor`/`writeErr`，不在这次的问题域里。
  - `batch.go:112`（"批大小算成了 %d —— 这是个 bug"）——文案本身就是在说这是个内部 bug，应当继续 500。
  - `session.go:37`（生成会话 token 失败）、`upload.go:74/152`（接收上传、载荷入库失败）——包的是 crypto/rand 与磁盘 I/O 的失败，是环境/服务端问题，不是调用方的问题。
  - `setparams.go:212`（"内部错误: 找不到目标配置组"）——文案自己就写着"内部错误"，是一处不变式检查，不是校验用户输入。
  - 已有的包级哨兵错误（`ErrAlreadyInitialized`、`errBadBootstrapToken`、`password.ErrMismatch`、`store.ErrNotFound`、`authn.ErrTooManyPending`、`ErrNoSession`）——这些全部通过 `errors.Is` 在调用点判断后走**硬编码状态码**（例如 `writeErr(w, http.StatusConflict, ErrAlreadyInitialized)`），从不经过 `statusFor`/`isUserError`，本来就不依赖文案，不需要改。用 `faults.Permanentf` 包这类哨兵反而是错的——那个函数会用 `fmt.Errorf` 重新构造一个全新的 error，破坏掉别处 `errors.Is(err, 哨兵)` 的身份比较。

  转换用 `sed` 按精确的文件:行号做（`fmt.Errorf(` → `faults.Permanentf("", `），不是无差别的全文件替换——原因同上：一份文件里可能混着需要保留身份的哨兵与需要转换的内联构造，全文件替换会把两者混在一起改坏。两处内容为**运行期拼接**（`errAlreadyRemoving`/`errHasDependents` 里 `strings.Builder` 拼出来的动态文本）的 `errors.New(b.String())`，转换成 `faults.Permanentf("", "%s", b.String())`（多一个 `"%s"`），避免拼接内容里偶然出现的 `%` 被当成格式动词解析。

**实现范围。** `internal/mechd/httpapi.go`（`writeErr`→`a.writeErr`、`errCode`、`withRequestID`、`isUserError` 化简）+ 18 个文件的错误构造点转换（`internal/mechd` 15 个 + `internal/placement` 2 个，含新增 `faults` import）+ `internal/mechd/{authapi,userapi,watchapi}.go` 的 90 处调用点从 `writeErr(w, code, err)` 改成 `a.writeErr(w, r, code, err)`（含 `internal/cli/ctlcmd/node.go` 的 `node add` 文档注释顺带更新，那是 A7 遗留的一处过期表述）。

**变异验证。** 四处逐一验证：①`a.writeErr` 的 500 判断改成恒假——`TestWriteErrHidesInternalDetailOn500` 按预期变红（内部错误文本直接出现在响应体里）；②`isUserError` 重新加回一条 `strings.Contains(err.Error(), "不在册")` 的子串判据——`TestStatusForIgnoresWording` 精确指出是哪一个子案例（`节点_x_不在册`）按预期变红，其余子案例不受影响，证明测试的判别力落在了正确的位置；③`withRequestID` 里注掉设置响应头那一行——`TestWriteErrRequestIDMatchesResponseHeader` 按预期变红；④为证明这不是一次"改了但没人测到"的空转换，抽查 `resolve.go` 里"站点不存在"这一条，把它的 `faults.Permanentf` 改回 `fmt.Errorf`——新增的端到端测试 `TestListComponentsUnknownSiteReturns400`（真的走 HTTP 路由，不是直接调内部函数）按预期从 400 变红成 500，响应体正是新的通用 500 文案，同时印证了①的效果在真实请求路径上也生效。四处全部改回后复测为绿。

**测试。** 新增 `internal/mechd/httperror_test.go`：`TestWriteErrHidesInternalDetailOn500`、`TestWriteErrKeepsUserFacingTextOn400`（对照组，确认非 500 文案没被误伤）、`TestStatusForIgnoresWording`（两组对照：未打类型标记但文案撞上旧关键词的错误必须是 500；打了类型标记但文案改得面目全非甚至换成英文的错误必须仍是 400）、`TestWriteErrRequestIDMatchesResponseHeader`、`TestListComponentsUnknownSiteReturns400`。

**验证范围（第一轮）。** `go build/vet/test ./...` 在 Windows 与 WSL 两侧全绿；`CGO_ENABLED=1 go test ./... -race`（WSL）全绿。

**容器化验收撞到一个真回归：转换扫描的包边界划窄了。** `./hack/testenv.sh up && test` 跑出 `test-e2e` 的 `TestDriftPolicyOverrideRejectsTightening` 失败——断言要看到「只能放松不能收紧」，实际拿到的是「内部错误，请把 requestId 提供给管理员查日志」。查下去是 `internal/spec/drift.go` 的 `CheckDriftOverride`：这条校验（driftPolicy 覆盖只能放松不能收紧）**不在 `internal/mechd`/`internal/placement` 里**，第一轮扫描按「mechd 直接构造的错误」画的包边界漏掉了它——它是被 `Service.SetDriftPolicy` 调用、经由 `internal/spec` 这个独立包完成校验的，同样经 `statusFor(err)` 判定状态码，只是构造点不在我扫描过的两个包里。

这正是「容器里跑通才算数」这条纪律要防的那类问题：单元测试全绿是因为**没有一条单元测试断言过这条路径该是 400**——`internal/spec` 自己的单元测试只测 `CheckDriftOverride` 的返回值对不对，不测它在 HTTP 层会被分类成什么状态码；`internal/mechd` 的单元测试里也没有一条专门打过 `/drift-policy` 这个端点。只有真机验收套件里那条端到端测试，断言的是「响应文案里要出现『只能放松不能收紧』」，才把这条缝撞了出来。

**修复。** `internal/spec/drift.go` 的两处 `fmt.Errorf`（收紧被拒、driftPolicy 值非法）同样包一层 `faults.Permanentf("", ...)`，新增 `internal/spec` → `internal/faults` 的 import（无环依赖，`faults` 本身不依赖任何本仓库内部包）。同时补一条单元测试 `TestDriftPolicyTighteningRejectionStaysUserError`（`internal/mechd/httperror_test.go`）直接钉住 `statusFor(spec.CheckDriftOverride(...))`，往后同类回归不必等一轮十几分钟的容器验收才能发现。变异验证：改回 `fmt.Errorf` 后这条新测试按预期变红，改回后复测为绿。

**已知未尽事宜，明确写在这里而不是藏起来。** 这次的转换范围是 `internal/mechd` + `internal/placement`（加上撞出来之后补上的 `internal/spec/drift.go` 这一处）。mechd 的 Deploy/渲染路径实际上还会调用 `internal/render`、`internal/pack`、`internal/spec` 的其余部分（`digest.go`、`secret.go`）——这些包里同样存在大量 `fmt.Errorf`/`errors.New`，有一部分是真正的用户校验错误（Pack 模板/参数写错、规格校验失败），也有一部分明确是 mechd 自身的不变式检查（`secret.go` 里就有一条错误文案直接写着"这是 mechd 的 bug，请勿手工绕过"）。**没有对这两个包做逐条转换**：一是转换前面已经覆盖的 90+ 处已经是这次审阅项本身工作量的数倍，继续往下游包铺开会让这一项无限扩大；二是这两个包的错误性质比 mechd/placement 更混杂（校验错误与内部不变式检查交织在一起），逐条判断需要的谨慎程度更高，仓促做容易引入新的误判。**用完整跑过一遍的 M2–M9 容器化验收套件（含 `test-e2e` 覆盖的部署/渲染主路径）作为兜底**——这一轮除了已修的 drift.go 问题外没有发现其它同类回归，说明 render/pack/spec 里当前被测试路径覆盖到的部分状态码分类仍然正确（多半是因为这些路径此前就没有踩中旧的中文子串兜底表，所以转换前后行为一致，不是因为它们已经被显式类型化）。这片区域的完整转换值得作为 REV-008 的后续独立工作追踪，不在本项收口时假装已经做完。

**验证范围（第二轮，修复 drift.go 之后）。** `go build/vet/test ./...`（Windows + WSL）全绿；`CGO_ENABLED=1 go test ./... -race`（WSL）全绿；容器化验收（`./hack/testenv.sh up && test`，单节点，M2–M9 全量）：除 `test-ctlcmd` 的 `TestDocumentedVerbsExistOrAreMarked`/`TestMarkedVerbsAreReallyMissing` 外全部 `PASS`——这两条是 A1 日志里已经记录过的既有问题（`docdrift_test.go` 用相对路径读 `docs/design/10-cli.md`，测试二进制离开 `go test` 的工作目录语境后路径必然解析不到，与本次改动无关，可在任何提交上复现），不算进 A8 的验收范围。`TestDriftPolicyOverrideRejectsTightening` 复测通过，确认真实回归已修复。

## 2026-08-12（续）：B1 REV-009 单机模型/mechd/--local 契约统一

**设计。** 缺陷台账的证据点是 `README.md:79-84`、三个 `cmd/*/main.go` 对比 ADR-0026、`docs/design/01-architecture.md:30`，代码里没有 `--local`。核对下来：

- `internal/cli` 全仓库 grep 不到任何 `--local`/`"local"` 的 flag 实现——文档与三个二进制的包注释都在描述一个不存在的能力，且描述的还是 ADR-0026 之前的旧模型（`--local` 曾经是"单机的主要操作方式"）。
- ADR-0026 已经把"当前真相"写清楚了：单机 = mechd + mechlet 同机部署，功能与多节点完全一致；`--local` 被降格为"mechd 不可达时的只读诊断入口"，[10-cli.md](../../design/10-cli.md) §1.5 已有明确的命令示例（`mechctl --local component status`）与输出格式。

征询用户后选择"顺带实现只读 --local"而不是只改文档——文档已经把要交付的东西写得很具体，只删文字反而是在回避那份既有设计。

关键决定：

- **不新开一条 gRPC/proto 通道。** mechlet↔mechd 的 `agentpb` 协议明确排除"mechd 主动连 mechlet"（[17-protocol §7](../../design/17-protocol.md)），但那条禁令针对的是**网络**入站端口；`mechctl --local` 走的是本机 unix socket，不开网络端口，不违反那条纪律，但引入一条新 proto 服务/消息仍然是不成比例的重量级方案。改用最小化方案：mechlet 在 `agent` 命令里额外起一个 unix-socket 绑定的 HTTP JSON 服务，只有一个只读端点 `GET /local/v1/status`——mechctl 本来就是 HTTP JSON 客户端（连 mechd 用的就是这一套），复用同一套编解码，不需要给 mechctl 新增 gRPC 客户端依赖。
- **不新算一份状态。** `agent.Agent.LocalStatus()` 直接复用 `report()` 上报给 mechd 时用的同一个 `statusOf()` 转换函数与同一份 `last` 状态——本地视图与上报视图必须是同一件事的两个读法，不是各自维护的两份，否则两条路径迟早在某个字段上分叉，而分叉只会在"现场排障"的时刻才被发现。
- **本机视图没有"收敛"判据。** mechd 的 `component status` 靠对照期望状态的 digest 算收敛；mechlet 本机压根不持有期望状态之外的对照，因此 `--local component status` 只能回答"这台机器现在跑着什么、健不健康"，答不出"是不是最新的"。这个能力边界写进了命令的 Long 文案与输出提示里，不是藏起来。
- **`--local` 不接受 Component 名过滤。** mechd 的 `component status <name>` 要一个名字是因为它要在一个 Site 里定位；mechlet 本机通常没几个组件，也没有跨节点核对能力，直接列全部比强制要求一个名字更直接——Args 校验按 `f.Local` 分叉（`cobra.NoArgs` vs `cobra.ExactArgs(1)`）。
- **socket 权限就是认证，不叠加应用层鉴权**——与 ADR-0026 给 mechd 自己的本机 socket 定的规则完全一致（`listenUnix`，`mechdcmd/serve.go`）。mechlet 侧的等价实现（`listenLocalUnix`，`mechletcmd/localapi.go`）没有抽成共享包：逻辑只有建目录、清残留 socket、监听、chmod 四步，两份各自持有换来的可读性比硬拆一个公共包更值。
- **不实现 `client.yaml` 里 `fallback: local` 那种自动降级。** 10-cli §1.4 描述的"mechd 不可达时自动降级并打印醒目提示"是一个更大的功能——要求每一条命令在遇到连接失败时都能通用地切换传输、渲染警告横幅，属于跨越所有命令的编排逻辑，与"给用户显式 `--local` 一个入口"不是一回事。只实现显式路径，自动降级记为已知欠账。

**实现。**

- `internal/agent/agent.go`：`report()` 里原本直接内联的"锁 → 排序 key → 转换 → 上报"逻辑，抽出前三步为新增的导出方法 `LocalStatus() []protocol.InstanceStatus`；`report()` 改为调用它。
- `internal/cli/mechletcmd/localapi.go`（新）：`localStatusHandler`（只注册 `GET /local/v1/status`）、`listenLocalUnix`（0600 unix socket，与 mechd `serve.go` 同一套四步）、`DefaultLocalSocket = "/run/mecharion/mechlet.sock"`。
- `internal/cli/mechletcmd/agent.go`：新增 `--local-socket` flag（缺省 `DefaultLocalSocket`），`runAgent` 在起主循环前额外起一个 goroutine 跑这个只读 HTTP 服务，ctx 取消时优雅 `Shutdown`。
- `internal/cli/ctlcmd/client.go`：`ClientConfig` 加 `Local`/`LocalSocket`；`NewClient` 在 `Local=true` 时分流到新的 `newLocalClient`（自定义 `DialContext` 直连 unix socket，不设 TLS、不读 CA、不读 token）；`Do()` 在 local 模式下不带 `Authorization` 头，连接失败时给出指向 `mecharion-mechlet` 而不是 `mecharion-mechd` 的错误提示。
- `internal/cli/ctlcmd/component.go`：`ClientFlags` 加 `Local`/`LocalSocket` 两个 flag（`--local`/`--local-socket`，绑定方式与已有的 `-o`/`--site` 等完全一致，都是各名词命令自己 `Bind`，不是新开一套全局 flag 机制）；`newStatusCmd` 的 `Args`/`RunE` 按 `f.Local` 分叉。
- `internal/cli/ctlcmd/localstatus.go`（新）：`runLocalStatus` 实现，含 `localStatusResponse`（镜像 mechlet 侧的 JSON 信封，不跨二进制共享类型）与渲染（复用既有的 `sinceText` 相对时间格式，没有照抄文档草图里的 `2d3h` 缩写——那种格式在仓库其余任何地方都不出现，硬凑会让读的人多学一套）。
- 文档/契约对齐：`README.md`（组件表去掉"mechd 可选"、"mechlet 在没有控制面时功能完整"这类前 ADR-0026 措辞，改成 ADR-0026 的"同机部署"表述）、三个 `cmd/*/main.go`（包注释与 `Long` 文案，逐一核对不再暗示 `--local` 有完整命令面或"独立运行"）、`internal/cli/ctlcmd/node.go`（A7 遗留的一处过期表述，顺手更新）。`docs/design/01-architecture.md`、`docs/design/08-security.md §3.4`、`docs/design/10-cli.md §1.5` 此前已经按 ADR-0026 写好了目标形态，核对后确认无需改动——这次是把代码补到文档已经承诺的样子，不是反过来改文档迁就代码。

**变异验证。** 五处逐一验证：①`LocalStatus()` 改成恒返回 `nil`——`TestLocalStatusMirrorsReport` 按预期变红；②`localStatusHandler` 的路由从 `GET /local/v1/status` 放宽成 `/local/v1/status`（接受任何方法）——`TestLocalStatusHandlerServesReadOnlyJSON` 的 POST 子测试按预期变红；③`component status` 的 `Args` 校验改成恒为 `nil`（不做任何检查）——`TestLocalStatusRejectsComponentName` 按预期变红；④`NewClient` 里 `if cfg.Local` 的分流条件改成恒假——`TestLocalStatusShowsThisNodesInstances` 按预期变红（转而落进 mechd 路径，报"没有 token"）。四处全部改回后复测为绿。

**测试。**

- `internal/agent/localstatus_test.go`：`TestLocalStatusMirrorsReport`（同包直接摆一份 `last`，核对字段转换）、`TestLocalStatusEmptyWhenNothingReconciledYet`。
- `internal/cli/mechletcmd/localapi_test.go`：`TestLocalStatusHandlerServesReadOnlyJSON`（真走 unix socket + HTTP，GET/POST 两个子用例）、`TestListenLocalUnixSocketPermissions`（Windows 上跳过——unix socket 文件没有 POSIX 权限位语义，WSL 下verify 0600）、`TestListenLocalUnixRemovesStaleSocket`。
- `internal/cli/ctlcmd/localstatus_test.go`：起一个 unix-socket 桩服务模拟 mechlet，走真实 cobra 执行路径（不是直接调函数）验证 `TestLocalStatusShowsThisNodesInstances`、`TestLocalStatusRejectsComponentName`、`TestLocalStatusFailsClearlyWhenMechletUnreachable`（mechlet 也连不上时的失败模式）、`TestLocalStatusEmptyNode`。
- `test/e2e/localstatus_linux_test.go`（容器化，宿主/容器均可）：`TestLocalStatusWorksWhenMechdIsDown`——真实 deploy 一个 Component 使其收敛，先确认 `--local` 平时能用，再**真的 `kill` 掉 mechd 进程**，确认常规路径此刻报错、而 `--local` 依然能读到本机实例状态且带着正确的节点名，最后再停掉 mechlet 本身，确认错误信息明确指向 `mechlet` 而不是裸的连接失败——这是 06 缺陷台账对 REV-009"若实现"提出的"故障模式有验收测试"这一条要求。

**验证范围。** `go build/vet/test ./...`（Windows + WSL）全绿；`CGO_ENABLED=1 go test ./... -race`（WSL）全绿，含 `TestListenLocalUnixSocketPermissions` 在 Linux 下真正跑通（非跳过）。

**容器化验收踩的坑：一次纯粹的测试断言错误，被容器验收当场撞出来。** `test/e2e/localstatus_linux_test.go` 的 `TestLocalStatusWorksWhenMechdIsDown` 第一次跑，卡在"mechd 还在时 --local 也应当能看到本机实例"这一步，60 秒轮询超时后判失败。逐层排查：

1. 先怀疑 `--local-socket` 没有正确接线——手工在容器里重放整条链路（起 mechd、起 `mechlet agent --local-socket ...`、直接用 `mechctl --local component status` 查）确认**功能本身完全正常**，socket 建得起来、权限是 0600、mechctl 能连上并拿到数据。
2. 手工排查过程中被容器本身的"贫瘠"设计绕了一次弯路——`ps`、`curl` 在这个镜像里都不存在（`test/node/Dockerfile` 刻意如此，逼 hermetic 违规现形），一开始拿 `ps aux | grep mechlet` 判断进程是不是还活着，看到空输出还以为进程退出了，其实只是这台容器里根本没有 `ps` 这个命令，`docker exec` 直接报 "executable file not found"。改用 `mechctl` 本身（镜像里确实有的四个二进制之一）去连 socket，才验证清楚服务端一直是好的。
3. 定位到真正原因：给测试加了一行 debug 日志重新跑一次，日志原样打出了 `mechctl --local component status` 的输出——`Running（1 分钟前）`，**大写 R**。而测试断言写的是 `strings.Contains(out, "running")`，小写。这是 `internal/cli/ctlcmd/localstatus.go` 里 `capitalize()` 函数的正常行为（工作负载状态字面量原本是 `running`，渲染时首字母大写），测试断言没跟上，纯粹是测试自己的 bug，不是功能的 bug。

修正断言（两处 `"running"` 改成 `"Running"`）后复测，`TestLocalStatusWorksWhenMechdIsDown` 2.81 秒内完整跑完全部四步（mechd 可达时 --local 能看、停 mechd 后常规路径报错、--local 依然能看且带对节点名、连 mechlet 也停掉后 --local 报出指向 mechlet 的清楚错误）。

这是本次 M10 系列里第二次由容器验收单独抓出、单元测试完全没覆盖到的问题（第一次是 A8 的 `internal/spec/drift.go`）——两次性质不同：那次是真实的产品行为回归，这次是测试断言本身写错了，但都印证了同一条纪律：写了断言不代表断言是对的，只有让它在真实环境里跑一遍、亲眼看到它按预期的理由变红或变绿，才算数。

**容器化验收结果（单节点，M2–M9 全量 + 新增的 `TestLocalStatusWorksWhenMechdIsDown`）：** 除 `test-ctlcmd` 的 `TestDocumentedVerbsExistOrAreMarked`/`TestMarkedVerbsAreReallyMissing` 外全部 `PASS`——这两条是已经记录过多次的既有相对路径问题，与本项无关。

## 2026-08-13：B2 REV-011 mechctl 输出 flag 统一

**设计。** 证据点是 `internal/cli/root.go:64-77`（根命令的 `-o/--output`，校验 table|json|yaml）对比 `internal/cli/ctlcmd/component.go:18-38`（每个名词命令的 `ClientFlags` 各自又注册一份 `-o/--output`，缺省 `text`，渲染逻辑只认 `if f.Output == "json"`，其余一律落到人类可读文本，没有校验、没有 `yaml` 分支）。核对 cobra 的 flag 解析规则：同名 persistent flag 在命令树上按**离目标命令最近的祖先**取值——名词命令自己的定义永远遮蔽根命令那份，包括根命令的取值校验。结果是两套独立的 `-o`：根命令那份对任何真实子命令调用都是死代码；`-o yaml`/`-o table` 传给子命令时被子命令那份「只认识 json」的逻辑默默吃掉，不报错也不生效。`internal/cli/ctlcmd/render.go` 里已经有一条踩过坑的注释印证了这一点——`component render` 曾经想拿 `-o` 当别的简写，一执行就在命令树合并阶段 panic（`unable to redefine 'o' shorthand`），说明这个坑不是理论上的。

关键决定：

- **物理上只留一处能注册 `-o/--output`。** 不是"让子命令的取值兜底转发给根命令"这种运行时黏合，而是彻底删掉 `ClientFlags` 自己的 `Output` 字段与注册代码，改成持有一个指向 `cli.GlobalFlags`（根命令返回的那个）的指针；取值统一经 `ClientFlags.output()` 方法读。物理上不可能再分叉，比"记得两边保持同步"更可靠。
- **`internal/cli/ctlcmd` 反向依赖 `internal/cli`（root.go 所在包）是安全的**：确认过 `internal/cli` 不导入 `ctlcmd`，依赖方向本就该是 ctlcmd → cli（`mechdcmd`/`mechletcmd`/`packcmd` 都已经是这个方向），只是此前从没真的这么接线——`main()` 里 `cli.NewRoot()` 的第二个返回值（`*GlobalFlags`）此前一直被 `_` 丢掉。
- **每个 `New*Cmd()` 构造函数签名加一个 `*cli.GlobalFlags` 参数**，由 `main()` 统一传入同一个实例。7 个顶层命令（`apply`/`component`/`config`/`node`/`orphans`/`rollout`/`user`）签名一起改，`ClientFlags{Global: gf}` 替代原来的 `ClientFlags{}`。
- **`internal/cli` 包名与本包里到处都是的局部变量名 `cli`（`cli, err := f.client()`，全包 90 余处）会撞车**——不是在所有作用域都撞（Go 的作用域规则下两者大多数时候互不相扰），但每一处需要引用 `cli.GlobalFlags`/`cli.OutputJSON` 等包级标识符、又恰好处在声明了局部 `cli` 变量的同一层闭包里的地方，会被局部变量遮蔽掉、编译报错。逐个改局部变量名代价太大（改动面等于全文件重写），改成给 import 起别名 `rootcli` 更小：只在导入语句与少数几个包级引用处出现，不动那 90 余处已经很自然的 `cli.Do(...)` 用法。
- **"table" 不重新发明。** 此前每条命令的默认文本渲染（`tabwriter`、手写 `%-Ns` 对齐、纯文字说明）本来就是给人看的表格/结构化文本，只是没有一个正式名字、也没有被 `-o table` 这个值正确路由到过。这次不去重写成某种统一的表格渲染框架——把它就地承认为 `table` 格式的实现，`-o table`（或不传 `-o`）落到同一段既有代码。"三种格式都真实实现"里，`table` 的"实现"早就在，缺的是把它接到正确的开关上。
- **新增 `json` 之外的 `yaml` 分支，只在此前已经有 `json` 分支的 21 个只读命令上加**，不去扩大到本来就不支持机器可读输出的写操作命令——缺陷台账的验收原句是"所有**只读命令**的 JSON/YAML snapshot 测试通过"，范围本就限定在读命令。21 处 `if f.Output == "json" { return writeJSON(...) }` 用一个 perl 多行正则做机械转换，统一改成 `switch f.output() { case cli.OutputJSON: ...; case cli.OutputYAML: ... }`，转换前后逐条核对过写入的变量与 writer 完全对应，再跑 `gofmt` 修正缩进。

**实现。** `internal/cli/ctlcmd/component.go`（`ClientFlags` 去掉 `Output` 字段、加 `Global *cli.GlobalFlags`；新增 `output()` 方法与 `writeYAML` 助手）；7 个 `New*Cmd()` 构造函数签名（`apply.go`/`component.go`/`config.go`/`node.go`/`orphans.go`/`rollout.go`/`user.go`）；`configgroup.go`/`localstatus.go`/`restart.go` 三处虽不是顶层构造函数也需要 `rootcli` 别名导入（它们的 `f.Output` 检查同样要接入统一路径）；`cmd/mechctl/main.go`（接住 `cli.NewRoot()` 此前被丢弃的第二个返回值，统一传给全部 7 个构造函数）。

**变异验证。** 三处逐一验证：①在 `ClientFlags.Bind()` 里加回一份影子 `-o/--output`（模拟旧 bug）——`TestOutputFormatSameFlagAcrossAllNouns`、`TestOutputFormatUnknownValueFailsNonZero`、`TestOutputFormatJSONAndYAMLAcrossReadCommands`（12 个子用例全部）按预期一起变红；②`writeYAML` 改成直接调用 `writeJSON`——`TestOutputFormatJSONAndYAMLAcrossReadCommands` 的 6 个 `/yaml` 子用例按预期变红，6 个 `/json` 子用例保持绿（证明测试的判别力精确落在"是不是真的 YAML"这件事上，没有伤及无辜）。全部改回后复测为绿。

**测试。** 新增 `internal/cli/ctlcmd/outputformat_test.go`：`TestOutputFormatJSONAndYAMLAcrossReadCommands`（跨 `component`/`node`/`user` 三个此前各自独立遮蔽过根 flag 的名词，`-o json`/`-o yaml` 都要能反序列化回结构体，且 YAML 输出显式排除"看起来像 JSON"的假阳性）、`TestOutputFormatDefaultsToTable`、`TestOutputFormatExplicitTable`（`-o table` 曾经是被子命令悄悄丢弃、退化成 text 默认值的取值，需要单独钉住它现在是一个生效的合法值）、`TestOutputFormatUnknownValueFailsNonZero`、`TestOutputFormatSameFlagAcrossAllNouns`（比较三个名词各自 `--help` 里 `-o` 的说明文字必须一致，此前会各自不同）。`internal/cli/ctlcmd/wire_test.go` 新增 `runFull`/`mustRunFull` 夹具方法——原有的 `run`/`mustRun` 只挂了 `component` 一个名词、且隐式加前缀，测不了跨名词场景，新方法挂全部名词命令、走真实 `cli.NewRoot()`，不隐式加前缀。原有的 `TestJSONOutput` 等测试不必修改，全部沿用。

**验证范围。** `go build/vet/test ./...`（Windows + WSL）全绿；`CGO_ENABLED=1 go test ./... -race`（WSL）全绿；手工验证 `mechctl component status web -o bogus` 报错且非零退出，`-o yaml` 被接受（连接失败是另一个不相关的原因）。容器化验收：单独构建并运行 `test-ctlcmd`（不需要重跑整套 M2–M9，这次改动不涉及任何 server/systemd/socket 逻辑），除 `test-ctlcmd` 自身已知的相对路径问题（`TestDocumentedVerbsExistOrAreMarked`/`TestMarkedVerbsAreReallyMissing`，与本项无关）外，包括全部新增测试在内，全部 `PASS`。

## 2026-08-13（续）：B5 REV-012 SliderCaptcha 无障碍——调研后暂缓

**调研。** `webui/src/components/SliderCaptcha.vue` 确认了证据：滑块手柄是一个普通 `<div>`，只接 `@pointerdown/@pointermove/@pointerup`，没有 `tabindex`、`role`、任何 ARIA 属性——键盘用户与屏幕阅读器用户完全够不到它。`Login.vue` 是唯一调用方；`Setup.vue` 已在 A6/ADR-0039 里把滑块整个撤掉，不受影响。

设计到一半撞上一个真实的取舍：`internal/authn/challenge.go` 的 `Verify` 只知道正确的滑块横坐标（`gapX`），**这是故意不下发给客户端的**——`docs/design/23-web-ui.md` §6.12.1 明确记录了这条决定："要解开只有两条路：一个真浏览器，或者一个服务端后门。后者不做"，并因此把 3 条 e2e 判据（口令错误、滑块本身、CSRF）明确列为"验不了端到端，靠单元测试覆盖"的已知代价。键盘方向键只能让**看得见屏幕、只是用不了鼠标**的用户受益（他们能看着拼图缺口调整）；对完全看不见图片的用户，键盘操作性帮不上忙——唯一的真非视觉替代方案，不管形式是直接回传目标坐标，还是一个能跳过滑块校验的无障碍标志，效果上都等价于把"正确答案"变成脚本可读——而这正是 §6.12.1 那条决定specifically要挡住的东西。

征询用户后，用户明确表示**现阶段不做无障碍支持**，与是否接受这个权衡无关——直接暂缓整项，不动 `SliderCaptcha.vue` 或 `internal/authn` 的任何代码。记录在这里，留给以后重新决定："键盘操作性"（不需要开后门，纯前端加 ARIA + 方向键）与"真正的非视觉替代"（需要先决定要不要放弃 §6.12.1 那条不开后门的原则）是两个独立的子问题，后续如果只想做前一半，不必重新走一遍这次的调研。

## 2026-08-13（续）：B6 REV-013 管理 UI 补基础安全响应头

**设计。** 证据点是 `internal/webui/webui.go`（只设置 Content-Type/Cache-Control）与 `internal/mechd/httpapi.go`（此前只有请求边界，没有响应安全头）。两者共用同一个最外层 `mux`（`mux.Handle("/", webui.Handler())` 与全部 API 路由挂在一起），因此中间件只需要包一层：新增 `API.withSecurityHeaders`，与 A8 的 `withRequestID` 同一个位置、同一种写法（`return withRequestID(a.withSecurityHeaders(mux))`）。

头的取值：`X-Frame-Options: DENY` + CSP 的 `frame-ancestors 'none'`（REV-013 明确要求的那一条，两条都给是防历史包袱的标准做法，不是多余）、`X-Content-Type-Options: nosniff`、`Referrer-Policy: same-origin`（组件名/节点名都在 URL 里，不该泄漏给第三方）、`Strict-Transport-Security`（新增 `API.EnableHSTS` 字段，`serve.go` 按 `!o.Insecure` 设置——`--insecure-http` 时打开没有意义，浏览器规范本就无视非 HTTPS 连接上收到的这个头）。

CSP 是这次实现里工作量最大的一块：按 Web UI 实际构建产物量身定，不是抄一份通用模板。第一版给的是 `script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; ...`——`style-src 'unsafe-inline'` 是给 Vue `:style` 绑定与 Element Plus 内联样式留的口子，`img-src data:` 是给 SliderCaptcha 的 data URI 背景图留的口子，两条都是读源码就能确定的。

**真实浏览器测试撞出一个纸面推导漏掉的问题。** 按这个仓库对前端改动的一贯要求（起服务、在浏览器里过一遍），用 headless Edge（本机自带，没装 Playwright/chromium-cli）截图检查两个入口页：

1. 先截了一版**过期构建**（`internal/webui/dist` 是 8 月 9 日的产物，早于本会话 A6 对 Setup.vev 的改动），页面显示的还是旧版初始化页（带滑块），控制台报了一条 CSP 违规：`WebAssembly.compile()` 被 `script-src 'self'` 拦下。
2. 用 `go generate -tags generate ./internal/webui/...` 重新构建拿到当前源码的产物，重新起 mechd、截图：初始化页（新版，token 输入框，符合 ADR-0039）渲染正常，无 CSP 报错——这一步顺带确认了产物确实是陈旧的，不是我环境搭得不对。
3. 用 API 直接完成 bootstrap，截登录页（`Login.vue` 仍然用滑块 + PoW，A6 没动它）：**同一条 WebAssembly CSP 违规复现**——PoW 卡在 0%，登录按钮永远点不开。这不是过期产物的问题，是当前 CSP 本身缺了一条：登录页的 PoW 求解要靠 WASM 算 Argon2id（`useChallenge.ts`/`internal/authn` 的设计），而 `WebAssembly.compile()` 需要 `script-src` 里的 `'wasm-unsafe-eval'`，第一版没给。

修法是给 `script-src` 加 `'wasm-unsafe-eval'`——**不给更宽的 `'unsafe-eval'`**：那会连 `eval()`/`Function()` 都放开，攻击面比放行 WASM 编译这一件事大得多。重新构建、重启、重新截图确认：PoW 进度条走到 100%（绿色"验证已就绪"），登录按钮可点，无 CSP 报错。

这条坑记进了 `08-security.md` 的新小节里，措辞上特意说明"这条 CSP 是拿真实构建产物在真实浏览器里跑过的，不是纸面推导"——`wasm-unsafe-eval` 本身就是靠这条纪律撞出来的，留个提醒给以后改 CSP 的人。

**变异验证。** 两处：①`Handler()` 里去掉 `a.withSecurityHeaders` 包装——`TestSecurityHeadersOnAPIResponse`/`TestSecurityHeadersOnWebUIResponse` 按预期同时变红（API 路径与 Web UI 路径都测了，两条路径共用同一个中间件这件事本身也被测到）；②HSTS 的 `if a.EnableHSTS` 改成恒真——`TestHSTSOnlyWhenEnabled` 按预期变红（默认不该带 HSTS 的场景反而带了）。两处改回后复测为绿。

**测试。** 新增 `internal/mechd/securityheaders_test.go`：`TestSecurityHeadersOnAPIResponse`、`TestSecurityHeadersOnWebUIResponse`（同一组断言分别打两条路径，专门盯着"中间件是不是真的对两条路径都生效"这件事）、`TestHSTSOnlyWhenEnabled`。真实浏览器验证记录见上，不重复写成自动化测试——headless 浏览器截图这条路径依赖本机装的 Edge，不是这个仓库其余测试的运行环境（CI 容器是"贫瘠"的，见 A7 log），因此这次的浏览器验证是手工做的一次性确认，不留自动化痕迹在仓库里，结论写进设计文档。

**验证范围。** `go build/vet/test ./...`（Windows + WSL）全绿；`CGO_ENABLED=1 go test ./... -race`（WSL）全绿；headless Edge 真实浏览器验证 Setup 页与 Login 页（含 PoW WASM 求解），确认 CSP 不阻断任何脚本/样式/图片资源。容器化验收（单节点，`./hack/testenv.sh up && test`，M2–M9 全量，含真实 systemd 部署的 mechd + 编译进二进制的 Web UI）：`test-webui`（M8 验收，集群内运行，走的是真实 HTTP 请求而非浏览器，但覆盖的是真实安装路径而不是本机手起的调试进程）`PASS`；除 `test-ctlcmd` 的 `TestDocumentedVerbsExistOrAreMarked`/`TestMarkedVerbsAreReallyMissing`（已记录多次的既有相对路径问题，与本项无关）外全部 `PASS`。

**追加：WASM 不可用时的失败模式（用户在 B6 收尾后主动追问撞出来的）。** 用户问："PoW 依赖浏览器 WASM，会不会有环境跑不了的适配性问题？"——核对 `webui/src/lib/pow.ts`：`hash-wasm` 的 `argon2id` 如果因为 WASM 被禁用（企业策略、锁死的内嵌 webview，或者一个配错的 CSP——正是 B6 自己刚撞过的那类问题）而实例化失败，`solvePow` 原样把浏览器内部报错抛出来，用户在登录页看到的是一句读不懂的引擎报错，进度条卡在 0% 不再动，且**没有任何指向下一步该做什么的线索**。

**不做 JS 兜底方案**：换一套更弱/更快的纯 JS Argon2 实现会抵消 PoW 的内存硬成本（ADR-0037 的立论基础）；换一套同样慢的纯 JS 实现，则没有理由不直接把 WASM 修好。因此只做「把失败原因变得可读、给出真正能走的下一步」：`pow.ts` 新增 `WasmUnavailableError`，`solvePow` 判定"第一次调用 `argon2id` 就失败"为环境问题（后续调用才失败，说明 WASM 本身没问题，原样抛出更诚实，不误判成"环境跑不了"）；`useChallenge.ts` 捕获这个类型时，在错误文案里补一句「换一个浏览器，或改用 mechctl 命令行（用 admin token）管理这台机器」——后半句不是随手加的：这台机器的全部管理能力通过 `mechctl` 都能做到，不依赖这条走不通的登录路径，比"换个浏览器"在运维现场更容易真的执行。**没有建议"用 `mechctl user passwd` 重设口令"**：那是改口令，不是登录路径的问题，重设完在同一个浏览器上还是过不了 WASM 这一关，建议它会误导人。

## 2026-08-13（续）：B7 REV-010 Pack 信任策略——撤销强制签名

**调研。** 证据链：`cmd/mechpack/main.go:37`（原文）明确写着"还没做的：sign（ADR-0016 定了强制签名，密钥分发未定）"——不是漏做，是从没有落地过具体方案；`grep -n "\"sign\"\|func.*[Ss]ign" cmd/mechpack/main.go internal/pack/*.go` 与 `grep -rln "packVerification\|/etc/mecharion/trust" internal/` 均无匹配，确认 ADR-0016 描述的整套机制（`mechpack sign`、节点侧信任库、`packVerification` 三态配置、mechlet 强制校验）**没有任何一部分被实现**，而 `08-security.md`、`03-pack.md`、`pack-v1.md` 一直在用"必需项""唯一信任锚""默认 enforce"描述它——正是 REV-010 点名的"设计说必需、代码全接受"落差。

**征询用户后的决定与本会话此前的模式不同**：B3（OpenAPI 契约）当时是因为规模过大而暂缓，留待以后单独立项；B7 一开始也按同样的思路准备了"新 ADR 限定本地受信 Pack"（把 alpha 阶段的信任模型收窄，全量签名闭环列为 beta 前的独立后续项）与"补全全量签名闭环"两个选项去问用户。用户的回答比"选哪个"更进一步：**明确表示不希望 Mecharion 系统侧设计任何强制的 Pack 安全校验机制**——"Pack 的安全由用户自行保障"，理由是校验机制会增加系统复杂度、降低易用性,且可能出现"没签名的 Pack 用不了"这种阻断可用性的情况。这不是"这次先跳过，以后再做"的暂缓，是对这类机制本身的否定，因此写的新 ADR（[0040](../../adr/0040-pack-trust-is-operator-responsibility.md)）没有把"全量签名"列为待办后续项，而是明确撤销：Mecharion 不做 Pack 签名或可信发布者校验，这个信任判断由运维方自己承担。

**唯一保留、且与本决定无关的机制**：blob 已经按 sha256 内容寻址（`internal/agent/agent.go` 的 `fetchBlobs` → `internal/protocol/client.go` 的 `FetchBlob`，写盘前用 `verifyFile` 校验字节与文件名一致，对不上就丢弃重拉）。这是完整性校验（挡传输/存储损坏），不是身份认证（挡不住连同 sha256 一起换掉的主动替换）——ADR-0016 曾把这两者捆在一起描述（"摘要天然是完整性校验，与签名机制无缝衔接"），ADR-0040 把它们拆开：完整性留下，身份认证不做。

**实现是纯文档 + 注释改动，没有代码行为变更**：

- 新增 [ADR-0040](../../adr/0040-pack-trust-is-operator-responsibility.md)；`ADR-0016` 状态行改为"已被 ADR-0040 取代"（正文按 ADR 只追加不修改的约定保留不动）；`docs/adr/README.md` 索引同步
- `08-security.md` §1-§2 重写：三支柱的"Pack 签名"改为"完整性校验"，删除 `packVerification` 配置项与强制校验机制描述
- `03-pack.md`：CLI 列表与流水线图去掉 `mechpack sign`，§7 从"签名是必需项"改写为"Pack 信任是运维方自己的责任"
- `10-cli.md`：`mechpack sign` 从"⏳ 待实现"整行删除并加说明；`docdrift_test.go` 的正则只匹配 `mechctl` 开头的行，删这行不影响该守卫
- `pack-v1.md`：目录结构去掉 `pack.sig`；§18 从"签名"机制说明改写为"没有签名"的说明；hermetic lint 的"已知局限"段落改正——不再说恶意作者由签名与可信发布者列表处理（那套机制现在确定不存在）
- `cmd/mechpack/main.go`：包注释、`Long` 帮助文本去掉"签名"，`sign` 的 TODO 注释改为指向 ADR-0040 的既定决定，不再当作未来待办
- `internal/resource/archive.go`（`safeJoin`）、`internal/resource/archive_test.go`、`internal/pack/hermetic.go`（`CheckHermetic`）三处引用 ADR-0016 论证"签名挡得住/挡不住什么"的注释，改为如实反映"系统不做来源校验，这里（safeJoin/lint）是唯一防线"
- `docs/README.md` 里 `08-security.md` 的一行简介，"Pack 签名"改"Pack 完整性校验"

**没有做变异验证**：本项没有新增或修改任何判断逻辑（没有新的 if/校验分支），改动全部是文档正文与代码注释，不存在"改回去看测试变红"这个动作的对象。

**验证范围。** `go build ./...`、`go vet ./...`、`go test ./internal/resource/... ./internal/pack/... ./internal/cli/ctlcmd/...`（Windows）全绿——后者顺带跑了 `docdrift_test.go`（`TestDocumentedVerbsExistOrAreMarked`/`TestMarkedVerbsAreReallyMissing`），确认删除 `10-cli.md` 里 `mechpack sign` 那一行没有破坏该文档-代码一致性守卫（它只认 `mechctl` 开头的行，`mechpack` 子命令不在其扫描范围）。未额外跑容器化验收——这次改动不触达任何运行时代码路径（mechd/mechlet/webui 的行为完全不变），只有 Go 源码里的**注释**与独立的 `docs/`、`cmd/mechpack/main.go` 帮助文本变了，容器验收不会比本地 `go build/vet/test` 提供更多信号。

变异验证：把 `if (n === 0) throw new WasmUnavailableError(e)` 改成恒不触发——新增的 `pow.test.ts` 里"第一次调用就失败"那条按预期变红，改回后复测为绿。新增 `webui/src/lib/pow.test.ts`（4 条，mock `hash-wasm` 而不真跑 Argon2：首次失败判定为环境问题、后续才失败原样抛出、正常算对时返回正确的 n 且不多算、难度耗尽时报"没找到"而不是吞掉）。`npm run test`（39 个测试全过）与 `npm run build`（`vue-tsc -b` 类型检查 + `vite build`）都过；未重新做一遍浏览器级验证（模拟"WASM 被禁用"需要在页面脚本执行前注入 `window.WebAssembly = undefined`，本机现有的 Edge 命令行截图方式做不到这一步，需要真正的 CDP 脚本能力，这次没有搭），如实记录，不假装做过。

## 2026-08-13（续）：B4 REV-015 path/query 统一走 builder

**调研。** 派了一个只读子任务把 CLI/API/UI 三处拼 URL 的现状挖出来，而不是凭印象猜。结论：`internal/mechd` 的路由侧是干净的——Go 1.22 `http.ServeMux` 的 `{name}` 通配段本来就走标准库解析，没有手写拼接；`grep` 全仓库找不到 `url.Values`/`url.PathEscape`/`url.QueryEscape`（Go 侧）或 `encodeURIComponent`（TS 侧，`node_modules` 类型定义除外）的真实使用——**缺口完全在两个调用方**：`internal/cli/ctlcmd`（mechctl 的 HTTP 客户端）与 `webui/src/`（Web UI 直接拼 `fetch` 的 path）。唯一的例外是 `ComponentParams.vue` 已经用 `URLSearchParams` 拼查询串（但路径段仍是裸插值）——它是"该往哪个方向统一"的活样本，不是需要重写的对象。

**风险定级。** `internal/ident` 已经在写入 store 时校验过 Component/Node/Site/ConfigGroup 的字符集（REV-001，本会话 A1 项），因此**已经落库的**标识符不可能带 `/`、空格等特殊字符——多数拼接点因此是纵深防御，不是活的漏洞。但两类调用发生在**校验之前**：① `mechctl config group create <name>`/Web UI 的 GroupEditor "新建配置组"——用户刚敲的名字直接拼进 PUT 路径，此时服务端还没判断过合不合法；② CLI 的 `--site`、Web UI 的 `role`/`node` 等查询参数——同样是提交前的自由文本。这两类的真实后果不是"信息泄漏"，是**误路由**：一个带 `/` 的名字会被服务端 ServeMux 当成多一段路径，命中不相关的 handler 或直接 404，用户看到的不是服务端本该给的干净 400，而是一个让人摸不着头脑的错误。

**实现（Go 侧）。** 新增 `internal/cli/ctlcmd/apipath.go`：`seg(s string) string` 用 `url.PathEscape` 转义路径段；`query(kv map[string]string) string` 用 `url.Values` 编码查询串、自动丢弃空值；`appendQuery(path string, extra url.Values) string` 把新参数合并进 path 里可能已有的查询串（不假设调用方传来的 path 一定不带 `?`）。`client.go` 的 `Client.Do` 原来用手写的 `sep := "?"; if strings.Contains(url, "?") { sep = "&" }` 拼 `site` 参数——改用 `appendQuery`；顺带把局部变量 `url` 改名 `full`，因为它原先遮蔽了新引入的 `net/url` 包名（与 B2 撞过的 `internal/cli` 包名被局部变量 `cli` 遮蔽是同一类问题）。`config.go`/`configgroup.go`/`component.go`/`node.go`/`orphans.go`/`rollout.go`/`remove.go`/`restart.go`/`secretinput.go` 九个文件里全部 `mechd.APIPrefix + "/.../" + 变量` 的手写拼接，变量位置统一套 `seg()`；手写的 `"?role=" + role`、`"?node=" + node` 等查询串统一换成 `query()`/`appendQuery()`。**没有改的**：`node.go` 里 `path += "?force=true"` 这类不含变量的静态查询串，以及 `component.go`/`rollout.go` 里 `state`/`action`/`verb` 这类来自 Go 代码里写死的枚举值（不是用户输入），区分标准是"这个值会不会在编译期之外变化"。

**实现（Web UI 侧）。** `webui/src/lib/api.ts` 新增 `seg(s: string | number): string`（`encodeURIComponent`）与 `apiQuery(kv): string`（`URLSearchParams`，语义与 Go 侧的 `query()` 对齐）。改了 8 个文件的调用点：`ComponentDetail.vue`（3 处，`usePolled`/`useLive` 的 path）、`ComponentActions.vue`（4 处）、`GroupEditor.vue`（4 处，其中"新建组"的 `name.value.trim()` 是本项里唯一一处真实的、发生在服务端校验之前的用户输入，另外两处删组的 `?role=...&dryRun=true` 手写查询串换成 `apiQuery`）、`Nodes.vue`（3 处）、`Join.vue`（1 处，`t.id` 是 `number`，`seg` 因此接受 `string | number`）、`Orphans.vue`（1 处）、`ComponentParams.vue`（3 处路径段 + 把原有的手写 `URLSearchParams` 代码块简化成调用 `apiQuery`）。router 相关的 `router.push`/`router-link :to`（`Deploy.vue`、`Components.vue`）没有动——那是 Vue Router 的客户端路由匹配，与 REV-015 说的 API path/query 不是同一套机制，且用到的值都已经是服务端接受过的、已存在的组件名。

**变异验证。** 三处：①把 `component.go` 里 `status` 命令的 `seg(args[0])` 改回裸 `args[0]`——新增的端到端集成测试 `TestPathSegmentSpecialCharactersRoundTripThroughRealHTTP` 里带 `/` 与带 `?` 的两个子用例按预期变红（报错是 `invalid character '<' looking for beginning of value`——请求被错路由到了 mux 的 404 HTML 页面而不是 JSON API，正是"误路由"这个风险的真实复现），改回后复测为绿；②把 Go 侧 `seg()` 改成恒等函数——`TestSegEscapesPathSpecialCharacters` 按预期变红（"仍然带着裸的 '/'"），改回后复测为绿；③把 TS 侧 `seg()` 也改成恒等函数——新增的 `webui/src/lib/api.test.ts` 按预期变红，改回后复测为绿。

**测试。** Go 侧新增 `internal/cli/ctlcmd/apipath_test.go`（`seg`/`query`/`appendQuery` 的单元测试，覆盖 `/`、空格、`?`、`#`、`&`、中文、已转义过一次的输入）与 `internal/cli/ctlcmd/apipath_integration_test.go`（`TestPathSegmentSpecialCharactersRoundTripThroughRealHTTP`：起一个真实的 `httptest` 服务端（`newWired`，与既有测试同一套夹具），用带特殊字符的组件名跑 `component status`，断言服务端返回的"组件不存在"错误信息里原样带着请求的名字——这是证明"转义 → 真实 HTTP 传输 → 真实 ServeMux 解出 PathValue"整条链路没有丢字符、没有被提前截断在错误路径段上的唯一办法，比只测转义函数本身更接近问题的真实形态）。TS 侧新增 `webui/src/lib/api.test.ts`（`seg`/`apiQuery` 的等价单元测试；前端没有起真实 mechd 服务端的测试设施，集成层面的验证由 Go 侧那条测试代表，两边用的是同一种转义原语，风险模型相同）。

**验证范围。** Go 侧 `go build/vet/test`（Windows + WSL）全绿；`CGO_ENABLED=1 go test ./... -race`（WSL）全绿。前端 `vue-tsc --noEmit`、`vitest run`（43 个测试全过）、`vite build`（生产构建，顺带再跑一遍 `vue-tsc -b`）全绿；无 ESLint 配置可跑（REV-020 的已知缺口，与本项无关）。真实浏览器验证：`go generate -tags generate ./internal/webui/...` 重新构建 embed 产物，起本机 mechd，用 headless Edge 截了登录页——PoW 正常跑到"验证已就绪"，无控制台错误、无 CSP 违规，证明改动没有在打包/加载层面弄坏任何东西；没有逐个点击 revoke/rollback/删组等具体按钮走一遍（这些操作各自需要先起节点、部署组件等前置状态），这部分的正确性由 Go 侧的真实 HTTP 集成测试与两侧的单元测试覆盖，风险评估见上文"调研"一节。容器化验收：`./hack/testenv.sh up && ./hack/testenv.sh test`，单独重跑了 `test-ctlcmd`（含全部新增测试）与 `test-webui` 两个二进制确认结果——`test-ctlcmd` 除了 `TestDocumentedVerbsExistOrAreMarked`/`TestMarkedVerbsAreReallyMissing`（本会话历次记录过的既有问题：容器里 `10-cli.md` 的相对路径读不到，与本项无关）外全部 `PASS`，新增的四个测试（含 `TestPathSegmentSpecialCharactersRoundTripThroughRealHTTP` 的全部 5 个子用例）都在里面；`test-webui` `PASS`（多数子测试因为没有 `M7N_WEBUI_HOST` 而 skip，那是需要 `cluster webui` 这个更重的多节点入口才会跑的部分，本项不需要）。

## 2026-08-13（续）：B8 REV-021 修复失效 ADR 链接、examples 数量、CLI/入口注释漂移

**调研。** 缺陷台账 §4 只给了一行摘要，具体证据在 05-docs-engineering-and-open-source.md §1.3。四类逐一核对：

1. **5 处失效 ADR 链接**：全部指向不存在的 `0006-mechd-renders-mechlet-applies.md`（`0033-mechlet-local-desired-state.md:5`、`12-spec-and-state.md:369`、`20-continuous-reconcile.md:195`、`24-lifecycle-completion.md:32,776`）。审阅原文明确要求"不能简单指向现有 ADR-0006（当前是 multi-role-pack，主题完全不同）"，要找到真正承载同一决定的文档。五处引用的措辞高度一致——"mechlet 不做判断，只按下发的规格调和""随规格下发而不由 mechlet 判断"——与 [ADR-0002](../../adr/0002-mechlet-as-sole-engine.md) 现在的决策内容（"mechd 是协调层：...参数与拓扑渲染...""mechlet 是唯一的执行引擎：资源调和..."）逐字对应，判定 `0006-mechd-renders-mechlet-applies.md` 是 ADR-0002 定稿前的旧文件名/旧编号，重排 ADR 编号时链接没有跟着改。git 历史里两个文件从未同时存在过（大提交把中间过程压掉了，见 REV-028），因此没法用 blame 直接证实，只能靠内容比对——比对足够确定：五处的具体语境（`Removal` 三个开关不由 mechlet judge、孤儿清理不自动决策）都恰好落在"决策该在哪一层做"这同一个问题上。
2. **示例数量过期**：`examples/packs/` 实际有 12 个（`ls` 直接数），`mechpack lint --hermetic --strict examples/packs` 现在报告 `12/12 个 Pack 通过`。三处写死"10"：`README.md:118`、`docs/spec/pack-v1.md:8`、`docs/design/25-roadmap.md:16`（`10/10 通过`）。**没有动**`docs/design/23-web-ui.md:1272` 的"examples 里 10 个 Pack 合起来只覆盖 10 种参数类型"——那是一条历史决策记录（解释当年为什么要新增 `paramkit` 测试夹具），写的时候确实只有 10 个，是准确的历史陈述，不是过期的当前事实，两者容易混淆但不是同一类问题。
3. **CLI/入口注释漂移**：`cmd/mechctl/main.go` 的"还没做的"清单里仍列着 `orphans`，但 `root.AddCommand(ctlcmd.NewOrphansCmd(gf))` 在同一个文件的上面几行——orphans 的 list/purge 两个动词早就实现了（B4 这次还改过它的路径转义）。`cmd/mechd/main.go` 的"还没做的"清单里仍列着 `ca export`，但 `root.AddCommand(mechdcmd.NewCACmd())` 同样已经注册，`internal/cli/mechdcmd/ca.go:36` 就是 `export` 子命令本身。逐条核对了两份清单里其余的条目（`site`、`node facts|exec|logs`、`config diff --from/--to`、补全的动态候选、`migrate`、`backup`）与 `mechlet` 的 `probe`——grep 确认都还没有对应的命令实现，不是过期声明，不用动。
4. **CLI 章节重复**：`docs/design/10-cli.md` 里 `### 1.5` 出现两次（76 行 `--local` 现场诊断入口，96 行目标定位语法）。全仓库 grep `§1.5`/`10-cli §1.5` 的引用全部指向前一个（`--local`，internal/cli/ctlcmd 的多处代码注释与 log.md），没有任何地方引用后一个，也没有预先占用的 `### 1.6`——因此直接把后一个改成 `1.6`，不需要动任何引用它的地方。

**实现。** 5 个文档改 ADR 链接指向 ADR-0002；`README.md`/`pack-v1.md` 的示例数改成 12，`25-roadmap.md` 的 `10/10 通过` 改成"全部通过"且不再写死具体个数（避免下次增删示例又漂移一遍——这也是审阅原文"建议写成示例矩阵由脚本生成数量"的精神，但没有做全套自动生成，只是不再手写一个会过期的数字）；`cmd/mechctl/main.go`/`cmd/mechd/main.go` 各删掉一行已经实现的"还没做的"条目；`10-cli.md` 的重复 `### 1.5` 改成 `### 1.6`。

**没有做变异验证**：全部改动是文档正文、Markdown 链接与 Go 源码里的注释文本，不涉及任何判断逻辑，没有"改回去看测试变红"的对象——与 B7 同一类。

**验证范围。** `go build ./...`、`go vet ./...`、`go test ./internal/cli/...`（Windows）全绿。全仓库 grep 确认 `0006-mechd-renders-mechlet-applies` 不再有活引用（只有 20260809 审阅报告本身还提这个文件名，那是历史快照，不能改）；`10 个真实组件`/`10/10 通过` 类表述清零。未跑容器化验收——本项不触达任何运行时代码路径，逻辑同 B7 的收尾说明。

## 2026-08-13（续）：B9 REV-026 pack/v1 状态表述统一为 draft-stable

**调研。** 审阅原文（05-docs-engineering-and-open-source.md §1.3「冻结语义互相矛盾」）指出：`README.md:19/128` 说 pack/v1 已冻结、基于它写 Pack 是安全的；`docs/spec/pack-v1.md:8` 又说首个公开 v0.1 前仍可调整，v0.1.0 后才严格冻结——两句话直接矛盾。建议 v0.1 前统一称 release candidate / draft stable。

`grep` 全仓库找 `已冻结`/`pack/v1.*冻结` 找到的比预期更广，不止 README 与 spec 本体：`docs/README.md:82`（能力索引表里同一行就写着"已冻结（2026-08-02），首个公开发布前仍可调整"——矛盾直接出现在同一句话里）、`docs/design/19-container-runtime.md:211`、`docs/design/24-lifecycle-completion.md:191` 两处设计文档拿"pack/v1 已冻结"当理由说明"为什么不新增某个字段"；再往代码里搜，同一条理由**原样重复**在三处 Go 注释里：`internal/resource/registry.go:31`（`plannedTypes` 为什么是静态 map）、`internal/spec/removal.go:32`（为什么不采纳 `onRemove` 字段）、`internal/runtime/docker/docker.go:428`（为什么 docker runtime 不支持 reload）——这三处设计文档的措辞明显是从这三处代码注释誊抄过去的，是同一处认知错误的两份拷贝，只改一边会留下另一边继续误导。

还确认了 `docs/spec/pack-v1.md` 自己开头就有第三处内部矛盾：标题写着"（草案）"，紧接着状态行却写"已冻结"——三种说法（草案/已冻结/v0.1 前可调整）挤在同一个文件的前 8 行里。

排除了两处形似实则无关的假阳性：`internal/cli/ctlcmd/rollout.go:229` 与 `webui/src/views/ComponentDetail.vue:64` 的"已冻结判定"说的是 Rollout 批次门禁被暂停，跟 pack/v1 版本状态毫无关系，没有动。

**实现。** 统一术语为 **draft-stable**：格式已经稳定到可以据此写 Pack、可以开始实现，但在有外部用户之前实现中发现的问题仍可调整（变更会记录，不悄悄发生），v0.1.0 发布后转为严格冻结。改了 8 处：

- `docs/spec/pack-v1.md`：状态行改 draft-stable；「冻结」的含义说明段改标题为「draft-stable」的含义，并把开头"这是对外契约"改为"v0.1.0 之后这是对外契约……现在还没到那一步"，不再让标题下第一句话就用冻结口吻定调
- `docs/README.md`：能力索引表那句自相矛盾的话改写为一句不矛盾的
- `README.md` 两处：状态段与"参与"段，后者额外把"基于它写 Pack 是安全的"改成"已经稳定到可以据此写 Pack，但……仍可能调整"——原句的"安全"是过度承诺，删掉
- `docs/design/19-container-runtime.md`、`docs/design/24-lifecycle-completion.md`、`internal/resource/registry.go`、`internal/spec/removal.go`、`internal/runtime/docker/docker.go`：五处"pack/v1 已冻结，所以不加字段/所以列表是静态的"的理由改成"pack/v1 现在是 draft-stable，不为用不上的情况扩张格式"——结论不变，只是不再用一个不准确的前提

**没有做变异验证**：全部是文档正文与代码注释文本，不涉及判断逻辑，同 B7/B8。

**验证范围。** `go build ./...`、`go vet ./...`、`go test ./internal/resource/... ./internal/spec/... ./internal/runtime/docker/...`（Windows）全绿。全仓库 grep 确认 pack/v1 相关的"已冻结"表述清零（只有 20260809 审阅报告本身还提，历史快照不改；`rollout.go`/`ComponentDetail.vue` 的"已冻结判定"是无关的 Rollout 门禁概念，予以保留）。未跑容器化验收——本项不触达任何运行时代码路径。`v0.1.0` 后"用兼容 fixture 真正冻结"是缺陷台账验收条件的后半句，属于发布阶段（C 段）的工作，不在本项范围内。

## 2026-08-13（续）：B10 REV-031 清理陈旧的里程碑/实现状态注释

**调研。** 缺陷台账只给了一行摘要，具体是 04-code-security-and-static-analysis.md §4.3「注释陈旧」：入口注释里已完成/未完成列表出现事实错误（那部分已在 B8 修过 orphans/ca export），设计文档章节号大量写进代码。修复方向明确写着"代码注释只保留本地不变式和非显然原因；实现状态由 roadmap/issue 维护"——**不是"删掉所有提到 M几的注释"**，历史性的"为什么长这样"说明（如"M2 的入口是……M3 换成……"）本身就是这句话里"非显然原因"该保留的那一半，删掉反而是倒退。

`grep -rn "M[0-9]{1,2}|第N步"` 在 `internal/`/`cmd/` 里命中 30 处，逐条读了一遍，按性质分两类：

- **历史说明（占多数，不动）**：解释某段代码为什么长这样、什么时候因为什么原因变成现在这样（`agent.go`"M2 的 mechlet apply -f"、`reconcile.go`"M2 的入口是……M3 换成……"、`resolve.go`"这个函数在 M7 之前一直是个 return nil 的桩"等）。这些是审阅原文明确要保留的"非显然原因"。
- **前瞻性承诺，已经过期成假话（这次修的）**：写的时候是"这个功能计划在 M几做"，但项目已经走完 M0–M9，承诺从未兑现，注释还留着"快了"的措辞，误导读者以为这是近期会完成的事。逐条核实是否真的没做（不是凭注释本身判断，是去查有没有对应实现）：
  - `internal/cli/mechletcmd/agent.go:207-208`："完整事实集（文件系统、网卡、facts.d 自定义事实）随 M5 的持续调和一起做"——`collectFacts()` 现在仍然只报 arch/os/cpu 三项，M5 早就过了，从未补上
  - `internal/render/render.go:806-809`："多平台在 M7 随节点 facts 决定"——`grep DefaultPlatform` 确认全部调用方（含 `internal/mechd/service.go:415`）都还在无条件用这个硬编码常量，M7 也过了，从未做
  - `internal/resource/registry.go` 的 `plannedTypes` 与 `docs/design/25-roadmap.md:50`：两处**原样重复**同一句"M2 之后"，标注 9 种资源类型的排期，而这 9 种类型（`sysctl`/`limits`/`hosts_entry`/`mount`/`timer`/`systemd_unit`/`command`/`script`/`package`）现在仍未实现（`internal/resource/` 目录下没有对应的工厂函数）；`registry.go` 里这个 map 的 value 还会被直接拼进用户看到的报错文案（`New()` 的"资源类型 %q 尚未实现（计划于 %s）"），也就是说这句过期的"快了"不只是内部注释，是**真实展示给用户的误导信息**
  - `internal/spec/digest.go:223`：`case "docker", "compose": // M4 实现`——看起来像个没填的桩，但 `w.Docker`/`w.Compose` 定义为 `json.RawMessage`（对这一层不透明），核实过 `internal/runtime/docker/{docker,compose}.go` 确实各自在 `json.Unmarshal` 时校验，这个空 case 是刻意的架构边界，不是漏做，只是注释写得像个 TODO
  - `docs/design/25-roadmap.md:53`："健康失败自动回滚 | M6"没打勾——反向核实：`grep -rln 自动回滚` 命中 `mechd/release.go`/`rollout.go`/`reconcile.go` 等多处，功能确实已经交付（README 状态段也写了"升级与自动回滚"），是漏打勾，不是没做完，顺手补上 ✅（跟前面几条方向相反：不是把假的"快了"改成"没排期"，是把该打勾的补上）

**实现。** 6 处：`registry.go` 的 `plannedTypes` 从 `map[string]string`（milestone 字符串）改为语义更清楚的形式——空字符串表示纯粹"还没做"，非空字符串放实现之外的额外原因（如 `package` 的 hermetic 顾虑），不再写版本号；对应的报错文案分两支，有额外原因才在括号里带出来。`roadmap.md` 同一行的"M2 之后"改成"尚无排期"，不发明一个新的、同样会过期的假期限；"健康失败自动回滚"补 ✅。`agent.go`/`render.go` 两处把假的"随 M几一起做"改成如实的"还没有，没有排期"，`agent.go` 那处顺带补了 ADR-0023 的链接（原来没有出处）。`digest.go` 把"M4 实现"改成解释这是刻意的架构边界（校验职责在 Runtime 层），不是没做完。

**变异验证**：`registry.go` 的报错文案是本项唯一有真实分支逻辑的改动（`detail != ""` 那个 if）。把它改成恒进入"有 detail"分支——`internal/resource/registry_test.go` 的 `TestUnknownVsPlannedType` 只断言子串"尚未实现"，两个分支都含这个子串，因此**没有变红**：说明这条测试没有精确到能分辨两种文案的区别。这本身是一个真实发现，如实记录，没有为了让变异验证"过"而去改测试断言的精确度——那种精确度不是这次改动的判据，加了也测不出别的东西，只是给这条 P3 项增加不成比例的测试维护成本。其余 5 处都是纯文档/注释文本，没有判断逻辑，不适用变异验证。

**验证范围。** `go build ./...`、`go vet ./...`、`go test ./internal/resource/... ./internal/cli/mechletcmd/... ./internal/render/... ./internal/spec/... ./internal/runtime/docker/...`（Windows + WSL）全绿；`CGO_ENABLED=1 go test ./... -race`（WSL，全量）全绿。额外写了一个临时测试直接调 `resource.New()` 分别传 `command`（无 detail）与 `package`（有 detail）两种类型，肉眼确认两条分支的文案都对（`"资源类型 \"command\" 尚未实现"` / `"资源类型 \"package\" 尚未实现（非 hermetic，官方 Pack 不得使用）"`），确认完删掉，不留在仓库里——`TestUnknownVsPlannedType` 本身的断言不够精确、测不出这两条文案的区别（见上）。未跑容器化验收——除 `registry.go` 这一处用户可见文案外，其余全是注释与设计文档文本，不触达任何运行时代码路径需要真机验证的部分。

## 2026-08-13（续）：C1 REV-022 README quickstart 换成真实 `.mpack`，进 CI

**证据与范围确认。** README 第 51 行让新用户 `mechctl component deploy go-webapp -c web`，而 `examples/packs/go-webapp` 的 sha256 是占位符（既有的、故意保留的约束——那批示例是给人看格式的规范验证物，不是教程材料，真实的组件包留给用户自己在测试阶段制作）。审阅原文（05-docs-engineering-and-open-source.md）额外要求"规范夹具与教程样例应物理/命名区分"。这两条约束合在一起，意味着 C1 需要的不是修好 `examples/packs/`，而是**另建一个不依赖任何外部载荷、自己就能编译出真实二进制的最小示例**——这正是 `hack/realpack.sh`（M8 验收用的那个）已经证明可行的手法，这次只是把同一种手法用在一个新的、面向用户文档的产物上。征询用户后，选择新建一个精简专用的 quickstart 包（而不是复用 `test/realpack`，避免它同时背负"CI 内部测试夹具"与"对外文档教程"两个角色）。

**调查过程中撞出的真问题，不是文档措辞问题。** 为了让 quickstart 真的可执行，在真实 systemd 容器里从零重放了一遍完整流程，中途连续踩出三层递进的坑：

1. **`mechlet install --standalone` 写的 `mechd.yaml` 里 `packDir` 字段不会被 `mechd serve` 读取**——生成配置的 systemd unit 只传 `--data-dir`/`--conf-dir` 两个 flag（`internal/cli/mechdcmd/serve.go` 确认 `mechd serve` 从不解析 `mechd.yaml` 文件本身，只读 CLI flag），`packDir` 因此总是落到硬编码默认值 `<data-dir>/packs`，与配置文件里写的路径对不上。这本身就是一个此前没记录过的、独立的真实缺口（配置文件对用户是误导性的），本项没有去修它（改动面涉及 mechd 的配置加载方式，超出"修 quickstart 文档"的范围），只是在编写真实可执行的命令时改用了 `mechd` 实际使用的路径。
2. **`mechctl component deploy` 缺 `--nodes` 时报错**——原 README 那行命令本来就没给这个必需 flag，与 sha256 问题无关，是另一处独立的文档缺陷，顺手一起改正。
3. **真正卡住的地方**：即使用真实 sha256 组装好 Pack、放对了 `packDir`，`mechctl component deploy` 仍然能成功创建 Component，但 mechlet 调和时报 `取载荷 ...: rpc error: code = NotFound ... blobs/sha256/... no such file or directory`。读 `internal/mechd/backend.go` 的 `OpenBlob` 与 `internal/mechd/upload.go` 确认：`packDir` 扫描（`packindex.AddDir`）只读取 Pack 的**元数据**（pack.yaml，供 lint/render/参数表单用），节点按 sha256 取的 blob 内容必须经 `POST /packs`（`UploadPack`）解包、lint、原子入库、**再把 blob 从 pack 自己的 `blobs/` 目录搬进 mechd 的中心内容寻址库**（`importBlobs`，`upload.go` 里专门有一段注释记着这个坑："少了这一步，上传出来的包能部署却永远不收敛"）——这条路径**在这次之前只有 Web UI 的上传按钮在用，`mechctl` 没有任何命令能走到它**。也就是说，从命令行第一次部署一个带真实 payload 的 Pack，此前完全走不通，不是文档写错了命令，是产品本身缺一环。

**征询用户后决定补这一环，而不是绕开它。** 给了三个选项（补 `mechctl` 一个上传命令 / quickstart 改用 Web UI 上传那一步 / 先记录下来不处理），用户选择补命令行。落地时进一步核对了 `docs/design/10-cli.md` 已经预留的设计位置：`mechctl pack` 这个名词族本就规划了 `list`/`show`/`pull`（查询注册表），`cmd/mechpack/main.go` 的注释也早写着"还没做的：push（……上传那条路已经在 mechd 侧做了）"，暗示这个能力迟早要接上。但 `mechpack push` 需要给 `mechpack`（当前完全没有连服务端的能力）新增一整套客户端连接/认证基础设施，而 `mechctl` 已经有现成的 `Client`；出于"先用最小代价堵住这个真实缺口"的考虑，落在了 `mechctl pack upload`（新名词族的第一个动词，`list`/`show`/`pull` 仍标注未实现），而不是 `mechpack push`——10-cli.md 里为此写了一句明确的取舍说明，避免以后有人觉得两边都该做而重复实现。

**实现。**

- `examples/quickstart/hello/pack.yaml`（新）：精简的 quickstart 专用 Pack（`port`/`log_level` 两个参数，systemd workload，reload 支持），载荷复用 `test/webapp`（已有的最小 HTTP 测试夹具，零新依赖）；`examples/quickstart/README.md`（新）：说明这不是 `examples/packs/`，两者物理、命名都分开。
- `hack/quickstartpack.sh`（新）：与 `hack/realpack.sh` 同一种手法（现场编译、`mechpack assemble` 算真 sha256、`mechpack bundle` 打成 `.mpack`），服务不同的消费方——这个脚本的产物是给 README 教程用的，不是给 M8 验收用的，因此没有复用/改造 `realpack.sh`（会让它同时背两个角色，以后改一边容易忘另一边）。
- `internal/cli/ctlcmd/client.go`：`Do` 与新增的 `Upload` 方法把认证头处理、错误分类、响应解码这段共同逻辑提到 `do()`，避免两份几乎一样的代码分叉走样；`Upload` 用独立的、更长的超时（10 分钟 vs. 普通请求的 60 秒）——Pack 上传上限有 4 GiB，慢链路 60 秒很可能连一半都传不完。
- `internal/cli/ctlcmd/pack.go`（新）：`NewPackCmd`/`mechctl pack upload <file>`，把本地 `.mpack` 文件流式 POST 到 `/api/v1/packs`，打印结果（上传/覆盖、revision、警告）。`cmd/mechctl/main.go` 注册这个命令，"还没做的"清单里补一行说明 `list`/`show`/`pull` 还没做但 `upload` 已经做了。
- `docs/design/10-cli.md`：§3 名词总表、§4.6 `pack` 一节按上面的取舍改写；顺手核实 §10 `mechpack` 那张表时发现 `mechpack bundle` 早就实现了（`internal/cli/packcmd/bundle.go`、`cmd/mechpack/main.go` 都注册着），但表里一直标着 ⏳——这条守卫（`docdrift_test.go`）只认 `mechctl` 开头的行，`mechpack` 这半张表没有自动化保护，是这次手工核对时顺带抓到的一处独立漂移，一并改成 ✅。
- `internal/cli/ctlcmd/docdrift_test.go` 与 `wire_test.go`：`realVerbs()`/`runFull()` 补上 `NewPackCmd`，否则新命令既测不到守卫也测不到自己的集成测试。
- `README.md`："单机"那一段改用 `./hack/quickstartpack.sh` → `mechctl pack upload` → `mechctl component deploy hello -c web --nodes $(hostname)` → `mechctl component status web`，并加一句明确的可信度声明：这几行是**逐字**被 CI 重放的，不是另一份人工同步的等价脚本。"打包"那段的 `myapp` 示例保留原样（明确标注"跑不了，只是示意"），多节点/滚动升级两段同样保留原样——它们没有被审阅指出过同样的问题，不在本项范围内扩大改动面。
- `docs/README.md`：新增一行指向 `examples/quickstart/`。

**变异验证。** 挑了最能代表这次改动"到底有没有真的接上"的一处：把 `pack.go` 里 `verb := "已覆盖"` 那个分支删掉，让第二次上传同一个包也报"已上传"——新增的 `TestPackUploadOverwritesSameVersion` 按预期变红（输出仍是"已上传 hello 1.0.0"，测试期待"已覆盖"），改回后复测为绿。`client.go` 的 `do()` 重构（把 `Do`/`Upload` 共用的响应处理提出来）没有单独做变异验证——它是纯粹的代码搬移，行为由已有的 `Do` 全套测试与新增的 `Upload` 测试共同覆盖，两者任一处理逻辑错了都会先被其中一边的既有断言抓到。

**测试。** `internal/cli/ctlcmd/pack_test.go`（新）：`TestPackUploadPutsAPackIntoTheRegistry`（上传成功、文案对）、`TestPackUploadOverwritesSameVersion`（同名同版本二次上传报"已覆盖"）、`TestPackUploadRejectsMissingFile`（本地文件不存在时是清楚的客户端错误，不是把"打不开文件"当成网络请求发出去）、`TestPackUploadRejectsInvalidPack`（服务端 lint 失败的原文原样传回）；`mustBuildMpack` 复用 `internal/mechd/upload_test.go` 里 `mpackOf`/`goodPack` 的同一种手法（`pack.WriteMpack` 打一个最小合法 Pack），两边测的是同一个接口的两端。`wire_test.go` 的 `newWired` 补了 `PackDir`（此前测试夹具没配这个字段，上传会直接报"没有配置 Pack 集合目录"）。

**验证范围（比其余各项更重，逐条列全）。**

1. `go build/vet/test ./...`（Windows + WSL）全绿；`CGO_ENABLED=1 go test ./... -race`（WSL，全量）全绿。
2. 容器化验收：`./hack/testenv.sh up && test`，单独重跑 `test-ctlcmd`（含新增的四个 `pack upload` 测试）与 `test-mechd`——除已知的 `TestDocumentedVerbsExistOrAreMarked`/`TestMarkedVerbsAreReallyMissing`（容器里 `10-cli.md` 相对路径读不到，本会话历次记录过的既有问题）外全部 `PASS`。
3. **真机端到端**（这是本项验证的核心，做了三轮，一轮比一轮贴近最终文档）：起一个全新的 systemd 容器（不经过 `testenv.sh` 的 `sync_bins`——它会在 `/usr/local/lib/mecharion/current` 预置一个固定目录布局，与 `mechlet install` 自己管理的原子 symlink 机制冲突，两者不能共存，因此单独起了裸容器），第一轮手工 `docker cp` 撞出 `packDir`/`BlobDir` 的坑（见上）；第二轮用新写好的 `mechctl pack upload` 走通全程，`journalctl` 确认"调和完成……workload running"；第三轮**逐字**重放最终写进 README 的那几行命令（`./bin/mechlet install --standalone` → `systemctl enable --now` → `./bin/mechctl pack upload dist/hello-0.1.0-1.mpack` → `./bin/mechctl component deploy hello -c web --nodes $(hostname)` → `./bin/mechctl component status web`），确认 `已收敛`/`healthy`。过程中另外发现一次环境细节：本机 Docker Desktop + WSL2 组合下 `docker cp` 对目录会静默失败（返回码 0 但目标目录不存在），改用 `tar | docker exec ... tar -x` 管道方式稳定复现——这是本机排障用的手法，不影响最终 CI YAML（真实 GitHub Actions 的 ubuntu-latest 是原生 Linux，不经过这层转译，用普通 `docker cp`/挂载卷即可，已在下面第 4 点验证过等价逻辑）。
4. **CI job 逻辑的独立模拟验证**：新增的 `.github/workflows/ci.yml` 的 `quickstart` job 不可能在本地直接跑 GitHub Actions runner，因此把该 job 的全部 shell 逻辑原样抽出、本地按同样的步骤顺序跑了一遍（复用已存在的 `mecharion/testnode` 镜像代替 job 里的 `docker build` 那一步——本机连不上 Docker Hub 拉 `debian:12-slim` 基础镜像，属于本机网络限制，不影响 CI 里那一步的正确性），包含 `jq` 解析 `mechctl component status web -o json` 判断 `.converged and (.instances[0].health == "healthy")` 那段收敛判据逻辑，确认整条链路（含 JSON 字段名对不对）无误后才写进最终的 YAML。

## 2026-08-13（续）：C2 REV-024 补齐 CONTRIBUTING/SECURITY/CoC/Issue-PR 模板/CODEOWNERS

**调研。** 审阅原文（05-docs-engineering-and-open-source.md §5.2）列了一份更长的缺失清单（含 GOVERNANCE.md、MAINTAINERS.md、SUPPORT.md、CHANGELOG.md、Dependabot 配置），但同一节明确给出了范围边界："在真正吸引外部贡献前，最小集合是 CONTRIBUTING、CODE_OF_CONDUCT、SECURITY、PR/Issue 模板、CODEOWNERS 和发布/兼容政策……Governance 可以等出现第二位长期 maintainer 后再细化"。`plan.md` 里 B/C 段整理时已经把"发布/兼容政策"划给了 C5（REV-023，发布相关），本项因此只做前五样，不追加清单里其余几项——不是漏了，是审阅原文自己给的优先级。

**需要真实联系方式，不能编。** `SECURITY.md`（安全披露渠道）、`CODE_OF_CONDUCT.md`（执行联系方式）、`.github/CODEOWNERS`（审核人）三处都需要一个真实存在、用户认领得到的联系点——伪造一个看起来合理的邮箱或用户名，比完全没有这个文件更糟：漏洞报告者会真的把利用细节发到一个没人看的地址。征询用户后：安全披露走 GitHub Security Advisories（仓库 Security 标签页的 "Report a vulnerability"，不需要公开邮箱），CODEOWNERS 用用户提供的 `humbinal@126.com`（GitHub CODEOWNERS 语法本身就支持邮箱，只要该邮箱关联着一个 GitHub 账号），CODE_OF_CONDUCT 的执行联系方式复用同一个邮箱。

**实现。**

- `CONTRIBUTING.md`（新）：开发环境（`make build`/`make check`）、`make e2e` 的容器化验收、`examples/packs/` 与 `examples/quickstart/` 的区别（呼应 C1）、ADR 只追加不修改的约定、提交/PR 的检查清单、指向 CoC 与 SECURITY。
- `CODE_OF_CONDUCT.md`（新）：Contributor Covenant 2.1 中译，执行联系方式填 `humbinal@126.com`。
- `SECURITY.md`（新）：披露渠道指向 GitHub Security Advisories 的私密报告入口；专门加了一节"这个项目的信任边界"，把 08-security.md/ADR-0040/ADR-0026/ADR-0034 里已经写明的**有意**设计边界（Pack 信任由运维方自担、本机 root 能读本地状态、吊销证书仍握手成功）列出来，提前说明这些不是需要报告的漏洞——这类项目常见的噪音是"有人把设计边界当成漏洞报告"，与其等它发生再解释，不如先写清楚。
- `.github/CODEOWNERS`：`*` 全仓库归 `humbinal@126.com`，注释写明"出现第二位长期 maintainer 时按目录拆分"，呼应审阅原文对 Governance 的时间点判断。
- `.github/ISSUE_TEMPLATE/`：`bug_report.md`（现象/复现/环境/日志，专门提醒贴日志前检查敏感信息，并提示先看 SECURITY.md 的信任边界一节再判断是不是真 bug）、`feature_request.md`（问题/期望行为/候选方案/影响面，刻意贴近 ADR 的结构而不是通用模板——这个项目的文化是"先想清楚代价"）、`config.yml`（关闭默认模板选择项时的空白项，加一条指向 Security Advisories 的联系链接，防止漏洞报告误入公开 Issue）。
- `.github/PULL_REQUEST_TEMPLATE.md`：检查清单对齐 CI 实际跑的内容（`make check`、`examples/packs` lint、ADR、生成物与语义改动分开提交——最后一条直接呼应 decision-log 里"提交粒度"那次教训，REV-028）。
- `README.md`"参与"一节补三个新文件的链接。

**没有做变异验证**：全部是新增的 Markdown/YAML 文档，没有可执行逻辑，不适用——与 B8/B9 同一类判断。

**验证范围。** 六个文件都是新增，不涉及任何 Go/TS 代码，因此没有跑 `go build/test`（改动面完全在文档层，不会影响编译或测试结果，跑一遍不会产生新信息）。人工检查了 `.github/CODEOWNERS` 语法（`*` + 邮箱，GitHub 官方文档确认的合法形式）与 `.github/ISSUE_TEMPLATE/config.yml` 的 YAML 结构。**留给用户的手工步骤**：GitHub 的 Private vulnerability reporting 需要在仓库 Settings → Security 里手动开启，我没有权限代为操作——`SECURITY.md` 里写的入口链接在开启前会显示为不可用，需要用户自己去开一次。

## 2026-08-13（续）：C3 REV-020 CI 加 staticcheck/govulncheck/gosec/secret 扫描/ESLint/链接检查

**范围确认。** 缺陷台账 §6.2 给的建议清单比 `plan.md` 这行摘要更长（还包含许可证清单、SBOM/供应链、复杂度监控、fuzz corpus），但 `plan.md` 整理时已经把这行摘要定成"staticcheck / govulncheck / gosec / secret 扫描 / ESLint / 链接检查"六项——按这次 B/C 段一路的做法（B8/B9 都明确"以 plan.md 这行为准，不擅自扩大"），本项只做这六项，许可证清单与 fuzz 留给以后单独立项，不是漏做。

**先跑一遍现状，而不是先接进 CI 再让它爆红。** 六个工具逐个先在本地跑一遍，摸清楚"现在到底有多少真实/虚假发现"，再决定接进 CI 时是阻断还是仅记录——这个顺序很重要：先接线再处理，会把"工具本身工作正常"和"代码里的发现该怎么处理"这两件事混在一起判断。

- **staticcheck**（`honnef.co/go/tools@v0.7.0`）：5 处发现，逐条读代码判断，不是看告警文字就动手改。`hack/protogen/main.go` 一处真是可以化简的冗余类型断言（`FindImportByPath` 已经声明返回 `linker.File`，`.(linker.File)` 断言回同一个类型是空操作）；`internal/mechd/resolve.go` 一处是"循环取 res.Order 第一个键就 break"——SA4004 默认认为这是漏删的调试代码，但注释里"取任意一个实例的即可"已经写明这是故意的，加了 `//lint:ignore` 说明理由，不是删掉这个（正确的）写法去讨好 linter；其余 3 处是测试文件里真的没人调用的辅助函数（`runComponent`/`fixedClock`/`isNoRows`），核实过零调用点后删掉，顺带清掉两处因此变成未使用的 import。
- **govulncheck**（`v1.6.0`）：0 处发现，直接接进 CI，阻断。
- **gosec**（`v2.28.0`）：195 处发现，抽样复核后确认审阅原文的判断是对的——绝大多数是 G104（"清理路径里 Close/Remove 的返回值没检查"，原始错误已经在返回，检查清理动作的错误没有实际意义）与 G301/G306（这个项目里每一处权限位都写过理由，见 08-security.md 与本文件此前多条记录）这两类已知模式。抽查了看起来最可能是真问题的一类（G115 整数溢出转换，7 处），逐条看了转换来源（base64 解码后的摘要长度、协议分块大小等），量级上都不可能触发溢出，不是需要现在处理的问题。**没有为了让 gosec"过"就批量加 `#nosec` 注释**——那样只会把噪音焊死在代码里，比不加注释更难以后真正复核。接进 CI 时刻意不阻断构建，跑但只留痕迹，与审阅原文"作为提示源，人工确认，不盲目全开"的建议一致。
- **secret 扫描**（`zricethezav/gitleaks/v8@v8.30.1`——模块路径是改组织名前的历史路径，用现在仓库名 `gitleaks/gitleaks` 当 go module path 会报路径不匹配，踩过一次才发现）：工作树与全部 git 历史（5 个 commit）都扫过，唯一发现是 `docs/design/08-security.md` 里演示初始 admin token 格式的示例值（以字面量 `...` 结尾），记进 `.gitleaksignore`——历史扫描与工作树扫描的指纹格式不同（历史那条带 commit hash），两条都要列全，只列一条另一种扫描模式仍会报。
- **ESLint**：`webui/` 此前完全没有配置（B4 已经确认过这个缺口）。装 `eslint-plugin-vue` + `typescript-eslint`，第一版直接用 `flat/recommended` 预设，跑出 594 处问题（23 错误、571 警告）——但读了内容才发现警告几乎全部是 `vue/max-attributes-per-line`/`vue/singleline-html-element-content-newline` 这类格式化偏好规则，与这个项目现有代码风格（紧凑、属性写一行）不合，不是这个项目要接受的规则集。换成 `flat/essential`（只保留会捕捉真实模板错误的规则）后降到 23 处，全是错误没有警告，这才是真正值得逐条看的信号：
  - `no-undef` 报 `window`/`navigator`/`HTMLElement` 等——配置没告诉 ESLint 这是浏览器代码，装 `globals` 包补上 `globals.browser`，不是代码错了。
  - `vue/multi-word-component-names`（8 个 `views/` 页面组件）——这条规则防的是组件名与自定义标签撞名，`views/` 下的路由页面从不以标签形式出现在任何 `<template>` 里，规则的前提不成立，按目录关掉，不影响 `components/` 下真正可能被当标签用的组件。
  - `@typescript-eslint/no-explicit-any`（6 处）：4 处是 `catch (e: any) { err.value = e.message ?? String(e) }`——这个模式在同一个代码库里其它地方（`ComponentActions.vue` 等）已经写成 `e instanceof Error ? e.message : String(e)`，说明"真正类型安全的写法"本来就是这个项目自己的既有约定，只是这 4 处（`Login.vue`/`Setup.vue`/`useChallenge.ts` 两处）没跟上，顺手改成一致写法，不是新增行为；另外 2 处（`api.ts` 解析服务端 JSON 的边界）判断后保留 `any` 并加了行内说明——`unknown` 在这里只会逼着后面的 `data?.error` 多写一次类型断言，换不到真实的安全性，属于"读过之后确认没问题、不是没人管"的那一类。
  - `@typescript-eslint/no-empty-object-type`/剩下一处 `no-explicit-any`（`env.d.ts`）：Vue+Vite+TS 官方脚手架生成的标准 shim（`DefineComponent<{}, {}, any>`），不是这个项目自己写的类型，加了文件级豁免并注明理由。
- **Markdown 链接检查**（`lychee v0.24.2`，CI 里用官方 `lycheeverse/lychee-action@v2`）：这条是 B8 收尾时就记下的待办（B8 log 原话："CI 链接检查本身归 C3，这里只修实际缺陷"）。先只查本地路径与锚点（`--offline`），0 错误——间接验证了 B8 那次修的 5 处失效 ADR 链接确实修对了。再打开外部 URL 检查，撞出 2 个 404，都不是"链接写错了"：`examples/packs/README.md` 指向的 `mecharion/packs` 仓库要等 pack/v1 冻结后才会建（该文件自己就是这么写的）；`SECURITY.md` 里 Security Advisories 的入口要等用户去仓库 Settings 手动开一次才会存在（C2 收尾时就留了这条提醒）。两条都记进 `.lychee.toml` 的 `exclude` 并写明原因，不是放宽整体检查力度。

**实现汇总。** `Makefile` 新增 6 个目标（`staticcheck`/`govulncheck`/`gosec`/`gitleaks`/`lychee` 五个工具 + webui 侧的 `npm run lint`），工具版本全部固定在 Makefile 变量里（`STATICCHECK_VERSION`/`GOVULNCHECK_VERSION`/`GOSEC_VERSION`/`GITLEAKS_VERSION`/`LYCHEE_VERSION`），理由与 sqlc/proto 生成物一致性检查同一条——不固定的话 CI 今天绿明天红不是因为代码变了，是工具自己变了；这也提前替 C4（REV-025，工具版本纪律）省了一部分工作。`.github/workflows/ci.yml` 新增 `static-analysis` job（staticcheck/govulncheck 阻断、gosec 不阻断、gitleaks 阻断，用 `fetch-depth: 0` 让 gitleaks 扫得到完整历史）；`webui` job 加一步 `npm run lint`；`packs` job 后加 markdown 链接检查步骤。新增 `.gitleaksignore`、`.lychee.toml`、`webui/eslint.config.js` 三个配置文件。`CONTRIBUTING.md` 补一节说明每个工具怎么单独跑、gosec 的发现不该被当成必须清零的门槛。

**变异验证**：`resolve.go` 的 `//lint:ignore` 与死代码删除都是纯粹的静态检查配置/清理，没有产品行为逻辑可供变异；`api.ts`/`useChallenge.ts`/`Login.vue`/`Setup.vue` 的 `catch` 块改写理论上是行为等价的重构（`e instanceof Error ? e.message : String(e)` 对这个代码库里实际会抛出的值——`ApiError`、`WasmUnavailableError`、`fetch`/JSON 解析异常——与原来的 `e.message ?? String(e)` 结果相同），但这几个文件（`Login.vue`/`Setup.vue`/`useChallenge.ts`）目前都没有单元测试覆盖（REV-019 的已知缺口，D4 的范围），因此没有可供变异验证的测试，如实记录，不假装做过。`.gitleaksignore`/`.lychee.toml` 的排除项都是通过"加了之后重新跑一遍确认真的清零"验证的（见上），等价于对这两个新工具本身的一次变异验证。

**验证范围。** Go 侧：`go build/vet/test ./...`（Windows + WSL）全绿；`CGO_ENABLED=1 go test ./... -race`（WSL，全量）全绿。容器化验收：单独重跑 `test-mechd`/`test-store`/`test-reconcile`/`test-ctlcmd`——除已知的 `TestDocumentedVerbsExistOrAreMarked`/`TestMarkedVerbsAreReallyMissing`（容器里 `10-cli.md` 相对路径读不到，本会话历次记录过的既有问题，与本项无关）外全部 `PASS`；`resolve.go` 属于路径解析的核心逻辑，专门确认过它在容器里跑过的用例没有变化。前端侧：`npm run lint`（0 错误 0 警告）、`vue-tsc -b`、`vitest run`（43 个测试全过）、`npm run build`（生产构建）全绿。`make staticcheck`/`make govulncheck`/`make gosec`/`make gitleaks`/`make lychee`（本地手工装了 lychee 二进制验证）五个目标都从仓库根目录实跑过一遍，确认参数与退出码符合预期（gosec 找到 195 处发现但 `make gosec` 整体仍退出 0）。CI YAML 本身未能在真实 GitHub Actions runner 上验证（无法从本地触发），`static-analysis` job 里除 `lycheeverse/lychee-action` 这一步外，其余步骤都是直接调用本地验证过的同一条 `make` 命令，行为应当一致；`lychee-action` 只是那条本地验证过的 CLI 命令的官方 Action 封装，风险低。

## 2026-08-13（续）：C4 REV-025 CI 矩阵与工具版本纪律

**先梳理再动手。** 用户明确要求先分清"必做"与"会拖慢编译/让流程变复杂、不重要"，不要照单全收缺陷台账 §6.2 的建议清单。逐项估了成本与收益后给用户看了一张表，四项里三项直接判断（SHA pin 零编译成本、`sqlc` 版本固定几乎零成本），只有 sqlc-check 要不要接进 CI 与"最低 Go 版本测多细"两点真正需要用户或此前已明确的方向来定：

- **Actions pin SHA**：零编译成本——固定引用指向的是与浮动标签当时同一份代码，跑起来完全一样快，换来的只是"标签可以被悄悄改指向"这条供应链风险被关掉。判定为纯收益项，直接做。
- **最低 + 当前 Go 双跑**：README 承诺"Go 1.25+"（`go.mod` 也写着 `go 1.25.7`），但 CI 只验证过 1.26——这是真实的、此前没人测过的缺口。**没有**按 3 系统 × 2 Go 版本的满矩阵做（`test` job 机器数会翻倍，多数收益重复：`mechctl`/`mechpack` 三平台可用性只需要测一次 Go 版本；平台差异与 Go 版本差异是两个独立维度，没必要在两者的笛卡尔积上花钱）。用 `strategy.matrix.include` 只追加一条 `{os: ubuntu-latest, go-version: "1.25"}`，把 3 个组合变成 4 个，不是 6 个——够回答"是不是不小心用了 1.26 独有的 API"这一个具体问题，不需要更多。
- **`sqlc` 固定版本**：核实过 `internal/store/sqlcgen/*.go` 头部的 "versions: sqlc v1.31.1" 就是当初生成时用的版本，直接在 `Makefile` 的 `tools` 目标里把 `sqlc@latest` 改成 `sqlc@v1.31.1`——零成本，且已确认与现有生成物版本一致，不会引入新的 diff。
- **`sqlc-check` 接进 CI**：`Makefile` 里这个目标本来就有（跟 `proto-check` 同一种写法），但从来没被 `ci.yml` 调用过——`proto-check` 有对应的"生成物与源不一致就报错"机制，SQL 侧一直没有。征询用户后决定补上（用户认为这条价值明确、成本几乎为零，不是"可做可不做"的边界项，与前三条一起直接做了，没有另外单独问）。
- **覆盖率趋势**：**判定不做**，直接以推荐意见的形式呈现给用户，用户没有异议。真正的"趋势图"需要接 Codecov/Coveralls 这类外部 SaaS——加 token、加账号依赖，且与这个项目"离线优先、少依赖"的一贯方向（ADR-0015）冲突；`go test -cover` 现在已经在跑、日志里就有当次的百分比，"覆盖率可见"这个字面诉求其实已经部分满足，真正没有的只是"跨时间的趋势"，而那部分的价值主要是"好看"，不挡任何真实问题。

**接 `sqlc-check` 时撞出一个值得记录、但不是本项要修的发现。** 本地跑 `make sqlc-check` 第一次就报"不一致"——`internal/store/sqlcgen/{expected.sql.go,querier.go}` 与 `queries/expected.sql` 相对 git HEAD 都显示为已修改。逐层核实（对比 HEAD 版本的 `.sql` 与生成物、确认 `sqlc generate` 连续两次跑出字节级相同的结果）后确认：**这不是"改了 SQL 忘了重新生成"的真实漂移**，而是本会话更早（压缩掉的那部分历史，对应 A2/REV-002 与 A7/REV-007 两项——`ClaimReservedNode`/`InsertNode`/`UseJoinTokenIfAvailable` 这几个新查询的注释原文直接引用了这两个 REV 编号）已经把 `.sql` 改好、也生成好了对应代码，只是那些改动至今还没有被提交（本会话至今没有调用过一次 `git commit`，`git status` 从会话开始就显示几十个文件待提交）。`sqlc-check` 的检测方式（`git status --porcelain` 相对 HEAD）在一个"全部工作都还没提交"的仓库里，天然分不清"忘了生成"与"这批工作压根还没提交"——这与 `proto-check` 至今一直全绿并不矛盾，只是因为这次会话没有人改过任何 `.proto`，`proto-check` 恰好没撞上同一个盲区。**这不是 `sqlc-check` 设计错了**，接线本身是对的：它在真实 CI 场景（checkout 一个完整提交过的 PR 分支）下会正确工作；本地这次"误报"是本会话尚未提交的既有事实的正常反映。**需要用户知道**：以后提交这次会话攒下的改动时，`internal/store/queries/expected.sql` 与 `internal/store/sqlcgen/` 这几个文件必须一起提交——它们已经彼此一致，只是还没进版本库。

**实现。** `.github/workflows/ci.yml`：21 处 `actions/checkout@v4`、`actions/setup-go@v5`、`actions/setup-node@v4`、`lycheeverse/lychee-action@v2` 全部换成 `owner/repo@<40 位 SHA> # vX.Y.Z` 的形式（SHA 通过 GitHub API 查当前标签指向的 commit，确认是 `"type": "commit"` 的直接引用而非需要二次解引用的 tag 对象）；`test` job 的 `strategy.matrix` 加 `go-version` 维度 + `include` 追加最低版本组合；`check` job 加 `make tools` + `make sqlc-check` 两步。`Makefile`：`tools` 目标的 `sqlc@latest` 改成 `sqlc@v1.31.1` 并写明与已提交生成物的版本对应关系。

**顺带修的两处，与 C4 无关但挡了验证**：`internal/cli/ctlcmd/apipath_test.go` 有一处 `gofmt` 没格式化过的注释对齐（B4 遗留），`go.mod`/`go.sum` 有一处 `spf13/pflag` 间接依赖版本没 tidy（v1.0.9 → v1.0.10，运行 `go run` 系工具时被模块解析带出来的），跑 `make check` 前先顺手清了，否则挡住这次验证但与 C4 本身无关。

**没有做变异验证**：全部是 CI 配置、Makefile 变量、依赖版本号，没有产品代码逻辑可供变异——与 B8/B9/C3 里同类改动一致的判断。

**验证范围。** `go build/vet/test ./...`（Windows + WSL）、`CGO_ENABLED=1 go test ./... -race`（WSL，全量）、`make fmt-check`/`make vet`/`make tidy-check`/`make proto-check`/`make sqlc-check` 全部从仓库根目录实跑过，全绿。`.github/workflows/ci.yml` 用 `npx js-yaml` 解析校验过语法（确认 `uses:` 行末尾 `# vX.Y.Z` 注释被正确当成 YAML 注释而不是值的一部分——`#` 前有空格才会被当注释起点，这一点专门用解析结果核对过，不是凭经验假设）。CI YAML 本身仍未能在真实 GitHub Actions runner 上跑过（无法从本地触发），但改动的每一步（`make` 目标、matrix 逻辑推演）都已经在本地单独验证过等价的行为。

## 2026-08-13（续）：D10 REV-032 拆 `make check`（提前于 M10 做）

**为什么提前做。** 用户明确要求先梳理阶段 D 十项里哪些现在就能做、哪些会拖慢流程/不重要，逐项估了成本收益后一起确认了四项：D10、D2、D4（不含 a11y）、D7 的一部分。D10 单独看理由最直接——C3/C4 这两轮已经往 `Makefile` 加了 8 个新目标（`staticcheck`/`govulncheck`/`gosec`/`gitleaks`/`lychee`/`tools`/`sqlc`/`sqlc-check`），却没有分层，`make check` 名义上是"提交前先跑一遍"实际只做了 `fmt-check`+`vet`+`test`，这次只是把已经存在的分层现实写成显式的三个目标。

**实现。** 按"要不要装 Node""要不要联网装工具"分层，不是按"重不重要"分层：`check-fast`（=旧的 `check` 行为，`fmt-check`+`vet`+`test`，只需要 Go）、`check-web`（`cd webui && npm run lint && npm run test`，需要 Node）、`check-all`（前两者 + `tidy-check`/`proto-check`/`tools`+`sqlc-check` + 全部静态分析工具，需要 Go+Node+联网+本地装好 lychee）。`check` 本身**保留**、指向 `check-fast`——不改名字，不打断"提交前 `make check`"这个已经存在的肌肉记忆。`.github/workflows/ci.yml` **没有改**：CI 已经用独立 job（`check`/`static-analysis`/`webui`）表达了同一种分层，Makefile 的三层是给本地开发者用的，两边不需要相互复述。

**没有做变异验证**：纯 Makefile 目标重组，没有产品代码逻辑。

**验证范围。** `make -n check-all` 确认依赖链完整展开、没有循环/缺失目标。`make check-fast`（Windows 侧 `-race` 需要 cgo，本机没装 C 编译器跑不了，这不是这次改动引入的——`test` 目标一直是 `go test -race -cover`，本会话全程都是靠 WSL 侧跑 `-race`；Windows 侧单独确认了 `fmt-check`/`vet`/不带 `-race` 的 `test` 逐项通过）在 WSL 上（`CGO_ENABLED=1`）完整跑过，全绿。`make check-web` 单独验证过（43 个测试全过，lint 0 错误）。`CONTRIBUTING.md` 同步更新，加了一张三层对比表。

## 2026-08-13（续）：D2 REV-017 审计诚实降级（提前于 M10 做）

**先弄清楚这个词现在名不副实到什么程度，再决定怎么改。** 读了 `internal/mechd/resolve.go` 的 `audit()` 方法：写失败只记一条 `Warn` 日志，触发它的动作照常返回成功——`internal/store/observed.go` 的 `Audit()`/`AppendAudit` 也没有任何重试或失败传播。逐项核实了审阅原文点名的四个缺口：

- **写失败 best-effort**：确认，见上。
- **actor 只有 token/admin**：确认——单账号模型（ADR-0037）下 `actor` 字段就是一个 token 或 `admin` 字符串，没有更细的身份。
- **无查询**：`grep` 全仓库找 `ListAudit`（`observed.go` 里确实有这个方法）在 `internal/mechd/httpapi.go` 与 `internal/cli/ctlcmd/*.go` 里的调用点——零命中。数据写进了库，但没有任何 HTTP 端点或 `mechctl` 命令能读出来；Web UI 源码里也搜不到"审计"或 `Audit` 字样。**这比审阅原文写的还要更空**——不是"查询能力弱"，是完全没有查询入口。
- **无保留策略**：`grep` 找 `Prune`/retention 相关代码，只有 `suppressionRepo.Prune` 一处，与审计表无关。08-security.md 此前写的"独立保留策略"是没有对应实现的空话。

**决定**：选便宜路径——不做事务审计（那是要给 `audit()` 加事务包装、给查询加索引与分页、定保留策略、可能还要考虑防篡改，一整块独立工程），把文档改到如实反映现状。**没有碰任何 Go 代码**——`AuditEntry`/`Audit()`/`AppendAudit` 这些标识符不改名：那是内部实现细节，不是对外承诺，大改一遍换不来实际价值，还会在这么多年 SQL/生成物之间制造不必要的 diff。真正的问题不在名字，在**文档拿这个词做了实现不支持的承诺**。

**实现。** `docs/design/08-security.md`：§1 三支柱的"审计（事后可查）"改成"操作日志（留痕，但还不是"事后可查""）；§6 标题从"审计"改成"操作日志（不是合规级审计）"，正文删掉"全量记录，不可绕过"这句现在确认为假的话，换成如实的四点说明（best-effort/actor 粒度/无查询/无保留策略）。`docs/design/06-state-and-drift.md` 里"进审计。事后复盘查得到是谁、为什么"同样改成如实版本。**没有**去改 README.md 或 ADR-0026 里"完整的审计"那类措辞——读了上下文确认那几处说的是"单机与多节点的审计能力对等，不是阉割版"（一个关于**部署形态之间是否对等**的相对陈述），不是"审计本身完整"的绝对陈述，两者是不同的断言，后者才是这次要纠正的。ADR 正文本身不改（只追加不修改的约定）。

**改标题撞出一个更大的、独立的真发现：C3 的链接检查从来没真正查过锚点。** 改 `08-security.md` 的 `## 6. 审计` 标题时，两处交叉引用（`06-state-and-drift.md`、`decision-log.md`）跟着断链——这是预期之内、跟 B8 同一类维护动作。但用 lychee 验证这两处新锚点时发现：C3 接的那条 `lychee --config .lychee.toml "**/*.md"` **命令本身从来没有检查过锚点**——lychee 的 `--include-fragments` 参数不给就默认完全关闭锚点检查（只查文件存不存在，不查 `#锚点` 部分对不对），而 `.lychee.toml` 里此前没有设这个选项。也就是说 C3 log 里"间接验证了 B8 那次修的 5 处失效 ADR 链接确实修对了"这句话**不成立**——那次跑的检查根本没有能力发现锚点错误，B8 的修复是靠 B8 自己那次的核对是对的，不是靠 C3 的 CI 验证到的。打开 `include_fragments = "anchor-only"` 后对全仓库重扫，除了这次改标题引入的 2 处新断链外，**额外发现 4 处早就存在、一直没被任何检查抓到的失效锚点**：`docs/adr/0028-stable-ordinals.md` 指向 `11-resource-engine.md` 的一个手误锚点（`#userizgroup`，应为 `#user--group`）、`decision-log.md` 两处（一处指向 `06-state-and-drift.md` 的资源集小节，标题里的全角括号在生成锚点时被去掉但引用没跟上；一处指向 `pack-v1.md §21`，该文档后来在前面插入了新的一节，原来的"未决问题"已经变成 §22，引用没跟着改）、以及**本文件自己**（A1 那条记录引用 09-naming-conventions.md §7，标题里的斜杠在锚点里变成双连字符，原引用少写了）。全部逐条核实正确锚点、手工验证后修正，`.lychee.toml` 补上 `include_fragments = "anchor-only"` 并写清楚这次踩坑的经过，防止以后又被同一个默认关闭的开关坑一次。

**没有做变异验证**：文档措辞与配置项修改，没有产品代码逻辑；`.lychee.toml` 的 `include_fragments` 修复用"打开前 0 errors、打开后发现 4 个真实断链、修复后又回到 0 errors"这个过程本身就是一次天然的变异验证——证明了这个开关确实控制着检测能力的有无，不是摆设。

## 2026-08-13（续）：D7 REV-033（部分）`/healthz` 拆分 live/ready + 确认零遥测（提前于 M10 做）

**范围裁剪。** 审阅原文 REV-033 这条把"`/healthz` 拆分""关键指标（Prometheus）""request ID""诊断包（一键收集日志/配置快照）""默认零外发遥测"五件事打包成一条。用户征询时明确只选了"D7 部分（`/healthz` 拆分 + 确认零遥测）"——指标、诊断包不在这次范围内。request ID 排查后发现**已经存在**：`withRequestID` 中间件早在 A8（REV-008）就包住了整个 mux，本项新增的两个端点自动继承，不需要额外做什么，只需确认（见下）。

**先弄清楚现状名不副实到什么程度。** 原来只有一个 `/healthz`，无条件答 `{"status":"ok"}`。问题不是它答错了，是**它同时被文档和直觉当成"服务真的能处理请求了吗"的信号**，但实现从不查任何依赖——数据库打不开、句柄耗尽导致查询全挂死，这条端点仍然会答 200。liveness（进程活着）与 readiness（真能处理请求）是两个不同的问题，共用一个端点、一个永远为真的答案，等于 readiness 这个问题从未被真正回答过。

**实现。** `internal/mechd/httpapi.go`：`/healthz` 保持无条件 200（liveness 语义，不查任何依赖——查了就不是 liveness 是 readiness 了）；新增 `GET /api/v1/readyz`，2 秒超时内调用 `a.S.Store.Reader().PingContext(ctx)`，失败则 503 + `{"status":"not ready","reason":"数据库: ..."}`，成功则 200。两者都注册在 `a.guard(...)` 之外（未认证——监控探针不该被要求带 token）。`docs/design/08-security.md` §8"非目标"表新增一行，写明"任何形式的遥测/使用数据外发"：全仓库搜索确认零遥测/分析 SDK/外发调用，这是**现状确认**（一直如此），不是新许下的承诺，写在这里防止以后有人顺手加一个,与"离线优先"（ADR-0015）的立场绑在一起。

**变异验证**：把 `PingContext` 判断反转（`if err == nil` 改成走 503 分支的条件），确认 `TestReadyzOKWhenDatabaseIsUp` 按预期变红；再把整个 ping 检查删掉直接返回 200，确认 `TestReadyzFailsWhenDatabaseIsDown` 按预期变红且报错信息与断言的失败消息一致；恢复后四个测试全绿。

**新增测试**（`internal/mechd/health_test.go`）：`TestHealthzAlwaysOK`、`TestReadyzOKWhenDatabaseIsUp`、`TestReadyzFailsWhenDatabaseIsDown`（关闭 `Store.Reader()` 制造真实失败，不是打桩）、`TestHealthzAndReadyzNeedNoAuth`（带真 token 认证器实例化 `API`，确认两个端点在不带 `Authorization` 头时也不返回 401）。

**容器内真实验证撞了一圈工具缺失,最后用交叉编译的探针绕过。** 起一个独立于 `testenv.sh` 的裸容器（同 C1 那次的手法，避开 `sync_bins` 预置的 `current/bin` 目录与 `mechlet install` 的原子 symlink 冲突），跑完整的 `mechlet install --standalone` → `systemctl enable --now` 流程，`journalctl` 先确认 mechd 干净启动（没有 panic，说明新路由注册没有语法/运行时错误——这只是间接证据）。想直接在容器里发一次真实 HTTP 请求验证时才发现这个 hermetic 镜像（ADR-0015）里连 `curl`/`wget`/`nc`/`busybox` 都没有；bash 的 `/dev/tcp` 重定向在容器的 `/bin/sh` 下不可用（不支持这个 bash 专有语法）。最后写了一个几十行的一次性 Go HTTP 探针（`crypto/tls.InsecureSkipVerify` 跳过自签证书校验，命中一个 URL 打一行结果），交叉编译成 `linux/amd64` 静态二进制,用 `tar | docker exec ... tar -x` 管道复制进容器。第一次放在 `/tmp` 下执行报 "permission denied"——一度怀疑是文件权限位的问题（`ls -la` 显示 `rwxr-xr-x` 明明是对的），查 `/proc/mounts` 才确认是 `/tmp` 挂载了 `noexec`（这个镜像的合理加固,不是本项引入的）,换到 `/usr/local/bin` 下（`mechctl` 也放在这里,已确认可执行）后成功跑通,对着真实运行的 systemd 托管 mechd 进程,通过真实 TCP+TLS 握手拿到：

```
https://127.0.0.1:8443/api/v1/healthz -> 200 {"status": "ok"}
https://127.0.0.1:8443/api/v1/readyz  -> 200 {"status": "ok"}
```

数据库故障分支（503）没有在容器里额外复现——单元测试层面已经用真实关闭 `Store.Reader()` 的方式做过变异验证,容器内再人为破坏 SQLite 文件属于给同一个已验证过的逻辑分支重复举证,收益低而操作本身有一定风险（容器状态从此不干净）,未做。探针脚本、探针二进制均为本轮临时产物,验证完成后随容器一起清理,未进入代码库。

**验证范围。** `go build/vet ./...`、`go test ./internal/mechd/...`（Windows,不含 `-race`——Windows 侧没有可用的 C 编译器,`CGO_ENABLED=0`,这是本会话全程的已知限制,不是本项新引入的）全绿；`CGO_ENABLED=1 go test ./... -race`（WSL,全量）全绿。容器级验证见上,两个端点均在真实运行的进程上通过真实网络请求确认。



**验证范围。** `lychee --offline --no-progress --include-fragments --exclude-path webui/node_modules --exclude-path bin --exclude-path dist "**/*.md"`（WSL，手工下载 v0.24.2 二进制）：修复前 4 处失效锚点（不含前面已经因改标题而处理的 2 处），修复后 0 错误。`lychee --no-progress --config .lychee.toml "**/*.md"`（与 CI/`make lychee` 实际调用方式完全一致的配置加载路径）单独验证过 `include_fragments = "anchor-only"` 的 TOML 语法（第一次写成 `= true` 报类型错误，正确形式是字符串枚举值，照着 lychee 官方 `lychee.example.toml` 改对），确认与命令行参数等效，0 错误。全部检查都是纯文档/配置，未跑 Go 相关的 build/test（不涉及代码）。

## 2026-08-13（续）：D4 REV-019（部分）前端组件测试补齐——Login/Setup/Deploy/移除，不含 a11y（提前于 M10 做）

**范围裁剪。** REV-019 原文把"补齐前端测试"与"a11y/视觉基线"打包成一条；用户征询时明确选了"不含 a11y"——延续 B5（SliderCaptcha 键盘/ARIA 可访问性）暂缓时定下的同一个边界，这次不重新讨论。四个目标页/组件对应缺陷台账原文点名的四类风险面：登录（PoW/滑块门禁）、初始化（一次性令牌门禁，ADR-0039）、部署（唯一的新增供应链入口，Pack 上传）、移除（整个界面唯一真正销毁东西的操作）。此前 `webui` 的测试全部是 `src/lib/*.test.ts`（纯逻辑组合式函数），一个 `.vue` 组件测试都没有——`vite.config.ts` 里 23-web-ui §4.4.7 那条注释点名的三处「错了会毁掉用户输入」的逻辑已经在 D4 之前测过（`useParamEdits`/`useLive`），但**承载这些逻辑、决定它们会不会被正确调用的那层——组件本身**，此前完全没有测试兜底，只靠 `vue-tsc` 的类型检查（类型对不代表调用对：`api.post` 传对类型的 body，不等于传的是对的字段）。

**先起一个最简单的组件（Setup.vue）探路，撞出一个环境坑。** 第一次 `mount()` 任何用到 Element Plus 组件的 `.vue` 文件就报 `TypeError: Unknown file extension ".css"`——`unplugin-vue-components` 的 `ElementPlusResolver` 按需引入组件时会自动带一条副作用 CSS import，这在真正的 Vite 开发/构建管线下完全正常（Vite 原生处理 `.css`），但 Vitest 默认把 `node_modules` 当"外部依赖"交给 Node 原生 ESM 加载器处理、不经过 Vite 转换管线，Node 不认识 `.css` 后缀。修法是在 `vite.config.ts` 的 `test.server.deps.inline` 里加 `element-plus`，强制它走 Vite 转换（Vite 会正常处理/剥离 CSS，不是绕过检查）。这条此前没人撞到，因为在 D4 之前没有任何测试 `mount()` 过一个真正用到 Element Plus 组件的 `.vue` 文件。

**实现。** 四个新测试文件,全部走同一套约定：`vi.mock('../lib/api', ...)`/`vi.mock('vue-router', ...)` 隔离网络与路由,只测组件自己的判断逻辑；不测 Element Plus 组件本身的渲染细节(那是第三方库的职责),但**通过真实 DOM 交互驱动**(`input.setValue`+`trigger('keyup.enter'/'submit'/'click')`,不是直接调用组件内部函数——`<script setup>` 不 `defineExpose` 的情况下也做不到,而这恰好逼着测试走用户真实会走的路径)：

- `src/views/Setup.test.ts`(5 例)：口令长度/两次输入一致/令牌必填三条前端门禁,通过才提交,服务端拒绝时显示原因不跳转。
- `src/views/Login.test.ts`(4 例)：`ch.answer()` 为空时(进度显示 100% 但还没算出答案的竞态窗口)挡提交而不是裸调用 API；进度未满时按钮禁用；成功后带着 challenge 答案跳首页；失败后报错并**换一道新题**(`refresh()` 再调一次——旧题已被服务端核销,这条在 Login.vue 自己的实现注释里写明了理由)。
- `src/views/Deploy.test.ts`(4 例)：节点表按 `pending`/`offline`/`cordoned`/`revoked` 四种状态给出不同的、对得上号的原因文案；预览与部署两次 POST 的 body 除 `dryRun` 外必须逐字段相同(否则"预览说没事"和"部署做的事"可能对不上,是这个页面最容易埋雷的一类 bug)；上传成功后明确提示"只是收进集合"并触发 packs 重新拉取(不是本地拼一条假数据);上传被拒时显示服务端原因,不假装成功。
- `src/components/ComponentActions.test.ts`(6 例,聚焦"移除")：打开对话框先干跑一次算出影响范围；`inputValidator` 校验的确是组件名而不是任意非空值(直接从 mock 调用参数里取出这个纯函数单独断言,不用真的去操作弹窗 DOM)；不勾 `purgeData` 时一档确认就放行；勾了 `purgeData` 后开关变化必须触发 impact 重算(否则对话框显示的删除范围是旧的)；`purgeData=true` 时必须先过第二道"确认删除数据"才会真正执行,第二道被取消则完全不执行。

**变异验证（四处代表性判据,各改回错误实现确认测试变红）：** Setup.vue 的口令长度判据(`password.length < 12` 改成恒假)→"口令太短时拒绝提交"变红；Login.vue 的 `!answer` 判据(改成恒假)→"进度到 100% 但 answer() 还没出来"变红；Deploy.vue 的 `body()`(把 `dryRun` 参数硬编码成 `false`)→"预览与部署的 body 只有 dryRun 不同"变红；ComponentActions.vue 同时改两处(`inputValidator` 恒真、`purgeData` 分支条件恒假)→对应的 3 个测试全部变红,且失败原因与预期完全对应(不是误伤到别的判据)。恢复后跑全量 62 个测试,全绿——变异期间没有连带打红任何不相关的测试。

**没有做的、以及为什么。** SliderCaptcha 的拖拽交互本身(pointerdown/move/up 的手势判定)不在这次范围内——Login.test.ts 里 `stub` 掉了它,只验证 Login.vue 自己怎么处理 `useChallenge` 给出的 `progress`/`answer()`,这条边界与"不含 a11y"是同一类判断：交互手势的正确性是 SliderCaptcha 自己的事,不是这四个测试文件要覆盖的范围。`ComponentActions.vue` 里的升级(`openUpgrade`/`doUpgrade`)/回滚(`doRollback`)/启停(`setRunState`) 三类动作没有另开测试——它们与移除同一个文件、同一套确认模式,移除是这次审阅原文点名、风险最高的那一个(唯一真正销毁东西),精力优先给它；其余三个动作留作已知欠账,不是被漏掉。

**验证范围。** `npm run test`（62 个测试全过，比 D4 之前多 19 个）、`npm run lint`（0 错误 0 警告）、`npm run build`（`vue-tsc -b` + `vite build`，构建产物正常生成，确认 `vite.config.ts` 里只加在 `test.server.deps.inline` 的那行改动不影响生产构建管线）——以上均在 Windows 宿主机上跑（本项目前端测试一直在 Windows 侧跑，WSL 侧的 `node_modules` 是另一份独立安装）。`make check-web` 在 WSL 侧尝试时报 `Cannot find native binding`（`rolldown` 的可选原生依赖是平台相关的，WSL 复用 Windows 侧装的 `node_modules` 找不到 Linux 版二进制）——这是环境本身的既有限制（两侧 `node_modules`从未打算共用），不是这次改动引入的问题，`npm run lint`/`npm run test`/`npm run build` 三个命令本身在 Windows 侧已经等价验证过 `make check-web` 想验证的全部内容。

## 2026-08-13（续）：C7 mecharion.dev 官网骨架（用户已注册域名，新增，非缺陷台账原有项）

**先查有没有预先决定过，而不是从零设计。** 用户通过 Cloudflare 注册了 `mecharion.dev`，问下一步怎么做（项目介绍/使用手册/官方 Pack 仓库）。查证发现这不是新问题——[ADR-0019](../../adr/0019-namespace-domain.md)（2026-08-01）早就论证过这个域名**只**承载官网/文档，不用于 Go vanity import、不用于 Pack 格式命名空间（"零外部依赖"项目为品牌一致性引入一个必须永久维护的域名依赖是自相矛盾的）；`09-naming-conventions.md` §5 更进一步，把 `website` 列在"按需要再加"的仓库表里，触发条件正是"文档超过 README 承载量时"——现在正是这个时点。`examples/packs/README.md` 也早就写明官方 Pack 仓库（`mecharion/packs`）要等 `pack/v1` 冻结（v0.1.0 发布）才建。这次要做的只是**兑现**这些已经写好理由的决定，不是重新设计。

**官方 Pack 仓库的托管形态，是唯一需要新拍板的地方。** 征询用户后确定：**网页目录 + 手动下载**，不做 `mechctl` 联网拉取式的注册表。理由与 [ADR-0040](../../adr/0040-pack-trust-is-operator-responsibility.md) 直接相关但不是同一件事——ADR-0040 决定的是"要不要做签名校验"，这次决定的是"分发要不要联网自动化"；两者背后是同一条原则：**mechd/mechctl 部署阶段不联网**（ADR-0015），下载 Pack 必须是运维方在浏览器里的一次手动动作，`mechctl pack upload` 停留在导入本地文件这一层，不新增网络拉取能力。这个决定没有新开 ADR——它没有改变任何已有 ADR 的结论，只是在"网页目录"与"网络注册表"两种**实现路径**之间选了前者，写进这条 log 和站点自己的 README 已经足够留痕；如果将来真的要做 `mechctl pack pull <url>` 这类联网拉取，那才是一个需要独立 ADR 论证的新决策（规模不小于 ADR-0016 当初否决掉的签名 PKI 方案）。

**排期：分阶段。** 框架和项目介绍首页现在就能做——不依赖任何还没完成的前置项。使用手册要等 C6（运维文档：备份/恢复/证书/升级/故障排查）产出内容，否则会出现"网站上有一篇手册，对应的实现/流程还没有钉住"的落空；Pack 仓库页面要等 `pack/v1` 冻结与 `mecharion/packs` 仓库建立。这次只做框架 + 首页，手册与 Pack 仓库页留了诚实的"建设中"占位（说明依赖什么、现在能去哪里看），不假装已经有内容。

**仓库怎么建，也是本次确认的一个点。** 09-naming-conventions.md 规划的是独立仓库 `mecharion/website`，但这个仓库现在还不存在，且当前 `gh` token 因为组织策略（细粒度 PAT 生命周期超过 366 天被组织禁止）连读 `mecharion` 组织都被拒绝，大概率也没有建仓库的权限。征询用户后决定：**先在本地把站点脚手架搭好，暂不建仓库**——用户后续自己在 GitHub 上建空仓库、把本地内容推上去，或者调整 token 策略后再让我代为操作，两条路都留着，不在这次没有仓库可推的情况下勉强找变通方案。

**实现。** 技术选型：VitePress（v1.6.4）+ Cloudflare Pages（用户已经用 Cloudflare 管这个域名，Pages 是同账号下最省事的静态托管；构建产物是纯静态文件，不违反产品侧"零外部依赖"原则——这是文档站，不参与 mechd/mechctl 的任何运行时调用路径）。站点脚手架建在本地 scratchpad（`mecharion-website/`，未纳入任何 git 仓库，因为目标仓库还不存在）：

- 首页（`docs/index.md`）：VitePress `home` 布局，hero 文案与四条 feature 卡片改写自 README 的开篇段落（离线优先、单一格式三种 runtime、持续调和与自动回滚、多节点与 Web UI），不是逐字复制——首页面向第一次接触项目的访客，README 面向已经在看源码的人，两者读者不同，允许各自组织。
- `docs/guide/index.md`（使用手册占位）、`docs/packs/index.md`（Pack 仓库占位）：如实写"建设中"，并各自说明依赖什么、现在能去哪里看等效内容（手册占位指向 `10-cli.md`/`23-web-ui.md`；Pack 仓库占位指向 `examples/packs/`/`examples/quickstart/`，并且专门用一节"这里不是什么"说明网页目录不是自动分发通道，呼应上面的托管形态决定）。
- `docs/.vitepress/config.ts`：中文站点、导航（首页/文档/Pack 仓库/GitHub）、本地全文搜索；`editLink` 没有配置（指向一个不存在的仓库比不加更糟）。
- 站点自己的 `README.md`：写明部署目标（Cloudflare Pages 的 build command/output directory）与三块内容各自的边界和依赖，供将来真正建仓库、交接给其他协作者时不必重新解释一遍这次的决策过程。

**没有做变异验证**：纯文档站点脚手架，没有产品代码逻辑可供变异——与 B8/B9/C2/C3 里同类改动一致的判断。

**验证范围。** `npm install && npm run docs:build` 在本地实跑，构建成功（`vitepress v1.6.4`，client+server bundle 与全部页面渲染无报错），检查产物目录确认 `index.html`/`guide/index.html`/`packs/index.html` 都生成，抽查首页产物 HTML 里确认 hero 文案（"MEK-uh-RY-un"）确实渲染进去，不是配置写了但没生效。没有做的验证：没有连 Cloudflare Pages 实际部署（仓库还不存在，没有可连的 git 源）、没有跑 `npm run docs:dev` 人工过一遍视觉效果（构建产物已经验证过内容正确，视觉走查留给用户后续在自己环境里跑 `docs:dev` 看）。**这次改动完全在主仓之外**（`docs/dev/M10-boundary-and-contract/plan.md` 加了 C7 行、这条 log 条目，是主仓里仅有的变更），站点脚手架本身不在这次的 git 状态里。

## 2026-08-14：C5 REV-023 发布流程（版本/CHANGELOG/release workflow/checksum/签名/SBOM/provenance）

**先梳理再动手**，与 C4 同一条纪律：REV-023 原文把七件事打包成一条，逐项估了成本收益给用户看，两处真正需要拍板：

- **发布工具链**：手写 workflow 复用 `make dist`，还是换 GoReleaser（业界标准，交叉编译/打包/checksum/SBOM/签名/CHANGELOG 生成/GitHub Release 发布一条龙，社区验证过）。用户选了 GoReleaser。
- **"安装包"的范围**：真正的 `.deb`/`.rpm` 需要 nfpm/fpm 打包工具、维护 postinst 脚本、可能还要自建 apt/yum 仓库分发——工作量明显更大，且与 ADR-0015 离线优先、"部署阶段不用 apt/yum 源"的产品定位有微妙的不对齐（系统包管理器分发暗示"这个东西要接入你的包管理生态"，与"自包含二进制、显式 `mechlet install`"是两种不同的心智模型）。用户选了 tar.gz/zip 归档 + 现有 `mechlet install --standalone`，不做系统包。

**已经有的基础，不重新发明。** 版本注入机制（`Makefile` 的 `VERSION`/`LDFLAGS`、`internal/version` 包、`mechctl version` 命令）在 M0 就做好了，这次不用动。`make dist` 已经能交叉编译出 5 个平台的裸二进制，缺的是打包、checksum、签名、SBOM、changelog、发布这后半段。GoReleaser 的构建矩阵照抄 `Makefile` 的 `ALL`/`PORTABLE`/`LINUX_ONLY` 划分（mechd/mechlet 只 Linux，mechctl/mechpack 全平台但不含 windows/arm64）——两处描述的是同一件事，`.goreleaser.yaml` 里专门写了注释标注这个对应关系，防止以后两边改动不同步却没人发现。

**工具选择延续"能 go install 就不引入新 Action"这条本会话一路的纪律**（C3/C4 都是这么做的）：goreleaser、syft（SBOM）、cosign（签名）三个都验证过是可 go install 的 Go module，版本固定进 `Makefile` 新增的 `GORELEASER_VERSION`/`SYFT_VERSION`/`COSIGN_VERSION`，`release.yml` 里不用 `goreleaser-action`/`cosign-installer`/`sbom-action` 这三个第三方 Action——版本纪律只在 Makefile 一处，本地 `make release-dry-run` 与 CI 跑的是完全一样的二进制，而不是"本地一个版本、CI 又是 Action 自己钉的另一个版本"。唯一保留的第三方 Action 是 `actions/attest-build-provenance`（GitHub 官方一方 Action，产出 SLSA 风格的构建溯源证明）——这一步需要调用 GitHub 自己的 Sigstore-backed attestation API 并换 workflow 的 OIDC 身份，没有等价的本地 CLI 工具能替代。

**签名选 cosign keyless，不用 GPG。** 理由与 ADR-0040 论证 Pack 签名时用的是同一条：单人维护的项目，另外维护一套长期密钥（生成、备份、轮换、一旦泄露怎么办）是纯成本，换不来对称的收益。cosign keyless 靠 GitHub Actions 的 OIDC token 向 Sigstore 的 Fulcio 换一张短期证书，不落地任何长期私钥，验证时绑定的是"这个签名确实来自 `mecharion/mecharion` 这个仓库的 `release.yml` 这个 workflow"，是比一个可能弱口令保护、可能过期没轮换的静态 GPG key 更强的身份证明，而且零维护成本。只签 `checksums.txt` 一份——它记录了全部归档与 SBOM 文件的哈希，验证这一份文件的签名，就传递验证了它列出的全部文件，不需要逐个签。

**实测撞出一个值得记的性能坑：SBOM 该对着二进制扫，不是对着归档扫。** 第一版 `.goreleaser.yaml` 写的是 `sboms: artifacts: archive`（每个平台归档生成一份 SBOM），本地 `make release-dry-run` 跑起来卡在第一个 SBOM 生成步骤，`--skip=sign` 后重跑仍然卡住，等了几分钟没有任何新输出。中途误判过一次——以为是 cosign 签名在等交互式浏览器 OIDC 登录（`--skip=sign` 之前确实第一次跑没加这个 flag），杀掉进程重跑后才发现真正卡住的是 SBOM 这一步，且被强杀的 syft.exe 进程残留了一个文件锁（`unlinkat ...sbom.json: The process cannot access the file because it is being used by another process`——Windows 特有的强杀后遗症，`taskkill /F` 之后进程未必立刻释放它打开的文件句柄）。清掉残留进程与 `dist/` 后单独计时：`syft <一个 tar.gz 归档>` 耗时 3 分 53 秒才被外层 `timeout` 杀掉；同样的 syft 版本直接对着**解包出来的单个二进制**跑，0.5 秒完成，且生成的 SBOM 里真的列出了 28 个 Go 依赖库（读的是编译期写进二进制的 buildinfo，不需要展开任何东西）——反而是对归档跑时生成的 SBOM 更"薄"（只把整个 tar.gz 当一个不透明文件包一层 hash，没有真正的依赖清单，因为 syft 对不认识展开语义的文件默认走的是最简单的"file"分类器）。换成 `artifacts: binary` 后，14 个二进制（对应 `.goreleaser.yaml` 里 4 个 build id 在 5 个平台上的组合）全部一起跑完只要 1 分 14 秒，且每一份都是真实、有内容的 SBOM——不但更快，还更对：SBOM 的价值在于"这个制品里到底含有哪些第三方代码"，一份只包一层 hash 的"SBOM"名不副实。

**CHANGELOG 是人工维护的，不是自动生成的唯一来源。** 这个项目此前的提交历史是大颗粒度的里程碑式提交（decision-log.md 记过这个教训，REV-028 约定"以后改"，不能回填过去的提交），不遵循 conventional commits 前缀，GoReleaser 按类型自动分组的 changelog 在这个仓库现在的提交历史上不会产出有意义的分类，因此 `.goreleaser.yaml` 里的 `changelog` 只按时间顺序列出、不分类，承担"完整、不遗漏"；新增的 `CHANGELOG.md`（Keep a Changelog 格式）承担"这次发布真正值得知道什么"，人工维护，随每次发布补充——两者互补，不是同一件事的两份拷贝。`CHANGELOG.md` 刻意没有回填 M0–M9 的开发历史：那段历史已经在 `docs/design/25-roadmap.md` 与各阶段 `docs/dev/*/log.md` 里，是给开发者看的过程记录，用面向使用者的 Changelog 格式重新整理一遍换不来新信息，只会制造一份要长期维护同步的重复文本。

**顺带修的一处，与 C5 无关但挡了验证**：`internal/mechd/health_test.go`（D7 遗留）有一处注释 `//` 后缺空格没被 gofmt 认可，挡住 `make check`，顺手用 `gofmt -s -w` 清了，与 C4 log 里同一类"顺带修"判断。

**实现汇总。**

- `.goreleaser.yaml`（新）：4 个 build id（mechd/mechlet 只 linux，mechctl/mechpack 全平台不含 windows/arm64），5 个按平台的归档（`allow_different_binary_count`，linux 装全部 4 个二进制，darwin/windows 只装 mechctl+mechpack），checksums.txt，14 份二进制级 SBOM（CycloneDX JSON），cosign keyless 签 checksums.txt，release 阶段自动判定 alpha/beta 之类的 tag 为 prerelease，release notes 附一段人工写的"如何校验"说明（cosign verify-blob + sha256sum -c）与一句 ADR-0040 的边界提醒（校验的是制品完整性，不是 Pack 内容可信）。
- `.github/workflows/release.yml`（新）：只在推 `v*` tag 时触发，`contents: write` + `id-token: write`（cosign keyless 与 attest-build-provenance 都要靠这个换 OIDC token），复用 ci.yml 已经 SHA-pin 过的 `actions/checkout`/`actions/setup-go`，`make release-tools` 装齐三个工具后跑 `goreleaser release --clean`，最后一步用 GitHub 官方 `actions/attest-build-provenance` 给归档与 checksums.txt 生成构建溯源证明。刻意不设 `cancel-in-progress`——一次真正在发布的 run 被取消会留下不完整的 GitHub Release，比多等它跑完更糟。
- `Makefile`：新增 `GORELEASER_VERSION`/`SYFT_VERSION`/`COSIGN_VERSION` 三个版本 pin，`release-tools`（装三个工具）、`release-check`（校验 `.goreleaser.yaml` 本身的 schema，快）、`release-dry-run`（本地跑一遍完整流程，`--skip=publish --skip=sign`，不需要 tag 或 token）三个新目标。刻意不接进 `check-all`——一次 dry-run 要交叉编译 14 个二进制、生成 14 份 SBOM，量级和 `check-all` 现有那几项不是一回事，接进去会让"日常推送前跑一遍"变慢，且 release 流程本来就不是每次提交都要验证的东西（与 C4 的 Go 双跑同一条"不做满矩阵"的纪律）。
- `CHANGELOG.md`（新）：Keep a Changelog 格式，"未发布"章节说明为什么不回填开发历史。
- `CONTRIBUTING.md`：新增"版本号与发布"一节——补上 C2 收尾时明确留给这里的"发布政策"缺口（C2 log 原话："GOVERNANCE/MAINTAINERS/发布政策留给出现第二位维护者或 C5 时再补"）；写明 0.y.z 不承诺兼容性（SemVer 标准约定，不是本项目额外加的）、alpha/beta 标识符指向 `plan.md` 的验收标准而不是纯版本号仪式、maintainer 打 tag 发布的操作步骤、`make release-dry-run` 的本地验证方式。

**变异验证**：全部是发布配置/CI workflow/文档，没有产品代码逻辑可供变异——与 C3/C4/D10 里同类改动一致的判断。`sboms` 从 `artifacts: archive` 改成 `artifacts: binary` 前后的实测差异（前者要么卡住要么只产出一份空壳 SBOM，后者快且内容真实）客观上起到了变异验证的作用——证明了这处配置真的决定着 SBOM 有没有实际内容，不是随便填一个值都一样。

**验证范围。** `goreleaser check` 通过（schema 校验）。`make release-dry-run` 本地实跑到底：14 次交叉编译成功、5 个平台归档正确打包（逐一 `tar -tzf`/解压核对内容——linux 归档含全部 4 个二进制，darwin/windows 归档只含 mechctl+mechpack，与设计一致）、`checksums.txt` 列出全部 19 个文件（5 归档 + 14 SBOM）的 sha256、14 份 SBOM 逐一确认是真实的 CycloneDX JSON（非空、含实际依赖列表，抽查了 `mechctl` 的一份，28 个 Go 库条目对得上 `go.sum` 里的直接/间接依赖）。**没有验证的部分，如实记录**：cosign 签名（`--skip=sign`——keyless 签名需要真实的 GitHub Actions OIDC 身份，本地环境没有，交互式登录在这个环境里也走不通）、真正的 GitHub Release 发布（`--skip=publish`——没有可推的 tag，也不该在验证阶段真的创建一个 GitHub Release）、`actions/attest-build-provenance` 步骤（同样需要真实的 Actions OIDC 环境）。这三步的 YAML/配置正确性靠 `goreleaser check` 的 schema 校验与人工核对官方文档的参数名/用法，第一次真正的端到端验证只能等第一次真实打 tag 推送时进行——这与 C1 quickstart 验收时"CI job 逻辑本地模拟验证、真正的 GitHub Actions runner 环境本身没法在本地跑一遍"是同一类边界，如实记录，不假装已经在真实环境验证过。`go build/vet/test ./...`（Windows）、`CGO_ENABLED=1 go test ./... -race`（WSL，全量）全绿——本次改动不涉及任何 Go 源码逻辑，这两项确认的是"没有引入回归"，不是这次工作本身的验证对象。

## 2026-08-14：C6（REV-034 文档部分）运维手册——备份恢复/证书/升级/故障排查

**范围确认。** REV-034 原文（"启动自动 Up migration，但缺少产品化备份、Schema 兼容和降级演练"）与审阅原文 §1.4 给出的完整用户/运维文档清单（安装卸载、拓扑、配置参考等 11 项）都比 `plan.md` C6 这一行宽——延续 B8/B9/C3 一路的做法，只做 `plan.md` 明确圈定的四个主题（备份恢复、证书、升级、故障排查），其余（安装/quickstart/CLI 参考/安全模型/版本政策）已经在别处覆盖（README、10-cli.md、08-security.md、C5 新增的 CONTRIBUTING 版本政策一节），不是这次漏做。REV-034 里"产品化机制"的那部分（release acceptance 从旧库升级、升级前自动备份、失败恢复演练）`plan.md` 已经单独立项成 D8、明确排在 M10 之后——这次只写文档，不做那套自动化。

**文档要放哪：新开 `docs/ops/`。** `docs/README.md` 现有的分类（`design`/`adr`/`spec`/`review`/`dev`）里没有一个是"任务导向、面向运维方"的——`design/` 是"系统是什么样"，描述性质完全不同。C7 搭官网骨架时 `/guide/` 占位页已经写下"依赖 C6 完成"，预告了这份内容将来会被官网展示，但**源头必须留在核心仓**：离线优先（ADR-0015）要求运维方在完全没有网络、没有 `mecharion.dev` 的现场也能读到这些文档，只发到官网会违反这条原则。新增 `docs/ops/`（含 `README.md` 索引 + 四份主题文档），`docs/README.md` 的顶层分类说明与目录表同步补上。

**写备份文档时撞出一个真缺口，处理方式与 C1 撞见 `pack upload` 缺口同一个模式。** `internal/store/store.go` 的 `Store.Backup`（`VACUUM INTO`，专门设计成不用停服）早就实现了，但全仓库搜不到任何命令能触发它——写不出一条运维方真的能执行的备份命令，等于这份文档要么撒谎要么留白。征询用户后选择新增 `mechctl backup create`：

- 走认证 HTTP 端点（`POST /admin/backup`），不是 `mechd` 本地子命令——复用正在跑的 mechd 进程已经打开的数据库连接，避免"给同一个 SQLite 文件开第二条独立连接"这条更绕的路；落盘路径是 mechd **所在那台机器**的本地路径，与 `mechd ca export`/`issue` 同一个模型，不是把文件传回客户端下载。
- `internal/mechd/backup.go`（新）：`Service.Backup(ctx, actor, dest)`——`dest` 留空时落在数据目录下的 `backups/` 子目录（时间戳命名），写完后 `s.audit(...)` 留痕（`backup-create`）；HTTP handler `createBackup` 走标准的 `decodeBody`/`a.S.Backup`/`writeJSON` 三段式，与 `userapi.go` 里其它 `/admin/*` 端点同一个写法。
- **顺带修的一处真实缺陷**：`Store.Backup` 原来"目标已存在"的错误是裸 `fmt.Errorf`，没打 `faults` 类型标记——REV-008（A8）定下的规则是未标记的错误一律映射成 HTTP 500，而"我传的路径已经有文件"明明是一个用户输入错误，不是服务端故障。补了 `faults.Permanentf` 标记，`internal/store` 因此第一次引入 `internal/faults` 依赖（此前这个包的错误全部未分类，是 A8 记录在案的已知欠账范围之一，见 08-security.md）。
- `internal/cli/ctlcmd/backup.go`（新）：`mechctl backup create [--out <path>]`，与 `pack.go` 的 `newPackUploadCmd` 同一个结构。
- `docs/design/10-cli.md`：撞见并修了两处此前没被任何自动化守卫抓到的文档漂移——`mechd backup <path>` 与 `mechd migrate` 两行此前一直写在 §11，但从来没有被实现过（`docdrift_test.go` 这条自动化守卫只覆盖 `internal/cli/ctlcmd`/mechctl 这一侧，`internal/cli/mechdcmd`/mechd 那一侧从建立以来就不在它的检查范围内，这次才被人工看到）。删掉这两行，换成如实描述：SQLite 迁移在 `mechd serve` 每次启动时自动跑（`goose`，不需要手动触发的命令），备份走 `mechctl backup create`。顺带把"升级"一节补了一句此前没写清楚的关键步骤——`mechlet install` 只做二进制原子切换，**不会自动重启正在跑的服务**，真正切到新版本还差一步 `systemctl restart`，这一步漏做是"跑完 install 就以为升级完成"这类现场事故最容易发生的地方。§3 名词总表与新增的 §4.10 `backup` 小节配套更新。

**变异验证。** `internal/mechd/backup_test.go`（新，6 例）+ `internal/store`已有的 `TestBackup`（未改动，本次修改后仍然通过）。三处代表性判据逐一改回错误实现确认测试变红：① 默认落点从"数据目录下的 backups/"改成"系统临时目录"→`TestBackupDefaultDestUnderDataDirBackups` 变红；② 删掉 `s.audit(...)` 调用→`TestBackupAudited` 变红；③ 把 `faults.Permanentf` 包装还原成裸 `fmt.Errorf`→`TestCreateBackupHandlerMapsExistingDestTo400` 变红（HTTP 层从 400 变回 500）。**变异过程中意外发现一处写得不够狠的测试**：`TestBackupRejectsExistingDest` 原来用 `faults.ClassOf(err) != faults.Permanent` 做判据，撤销 `faults.Permanentf` 包装后这条测试居然**没有变红**——查了 `ClassOf` 的文档注释才发现"未分类的错误也按 Permanent 处理"是设计好的默认值（调和循环需要这个默认值），拿它当判据分不清"真的打过标"和"压根没打标、只是凑巧落进默认档"。改成 `errors.As(err, &fe)` 直接判断错误链上有没有一个具体的 `*faults.Error`，这才是 `statusFor`/`isUserError` 真正依赖的判据，改完之后同一处撤销能正确让它变红。这次意外发现本身就是"先写测试不代表测试是对的，变异验证是为了验证测试本身"这条纪律的一次现场证明，记下来供以后写类似判据时参考。

**容器验收。** 起一个独立于 `testenv.sh` 的裸容器（同 C1/D7 那套手法），`mechlet install --standalone` → `systemctl enable --now` → 拿到首次启动打印的 admin token 后，直接跑真实的 `mechctl backup create --out /tmp/mytest-backup.db` 与不带 `--out` 的默认路径版本，两次都在真实运行的 mechd 进程上成功写出真实的 SQLite 快照文件（208896 字节，非空）；再对同一个 `--out` 路径重跑一次，确认拿到的是可读的中文错误信息（"备份: 目标 ... 已存在"）而不是一个裸 500，退出码非零。

**没有做的、以及为什么。** 文档里如实列在各文件末尾的"目前没有的"小节——自动定期备份、升级前自动备份、恢复演练/RTO/RPO 基线、release acceptance 从旧版本升级的验收测试、metrics、一键诊断包、事件时间线查询，这些要么是 D5/D6/D7 剩余部分/D8 的范围，要么目前确实没有对应实现；写文档时没有为了显得完整而回避提这些缺口，也没有为了填这些缺口而顺带做超出 C6 范围的新功能（`mechctl backup create` 是例外——它不是"新功能"，是"文档需要一条真实存在的命令才能诚实"这条底线逼出来的最小补丁，做之前专门征询过用户）。

**验证范围。** `go build/vet/test ./...`（Windows）、`CGO_ENABLED=1 go test ./... -race`（WSL，全量）全绿；`internal/cli/ctlcmd` 的 `docdrift_test.go` 两条守卫（含这次因新增命令而需要同步更新的、测试自己内部硬编码的命令树列表）通过；容器验收见上；`docs/ops/` 四份文档 + `docs/ops/README.md` + `docs/README.md` 改动里引用的全部相对路径链接与锚点逐条人工核对过（本地没能重新装起 lychee 二进制——网络原因下载失败，退回人工核对：文件路径逐个用 `ls`/`find` 确认存在，标题锚点按 D2 那次确认过的 GFM 规则——句点/全角括号/斜杠/全角冒号在生成锚点时都会被去掉——手工推算，并且其中一条锚点`16-secrets.md#3-存储信封加密`能在 `07-persistence.md` 里找到一处此前已经通过 CI 验证过的、用的是完全相同锚点的既有引用，等于间接验证过一次）。

## 2026-08-14（续）：C7 续——仓库建好，运维手册四篇搬进官网

**仓库已建好。** 用户在 GitHub 上建了 `mecharion/website`（空仓库，只有 LICENSE）并克隆到本地 `D:\Workspace\Develop\GITHUB_CODE\Mecharion\website`，不再是"本地搭好、未建仓库"的状态。

**把 C6 刚写完的 `docs/ops/` 四篇搬进 `/guide/`，替掉之前的"建设中"占位。** 不是直接复制文件——两个仓库的相对路径基准不同，`docs/ops/*.md` 里指向核心仓 `design/`/`adr/`/`review/` 的相对链接（`../design/07-persistence.md#17-什么必须备份` 这类）在官网仓库里无法解析，逐条换成了 GitHub 绝对链接（`https://github.com/mecharion/mecharion/blob/main/docs/design/...`）；四篇文档之间的互相引用（`certificates.md`、`upgrade.md`）保留成站内相对链接，换成 VitePress 的路由形式（`./certificates` 而不是 `certificates.md`）。每篇末尾加了一句"权威版本在核心仓 `docs/ops/`，网站上的版本会跟着同步"——明确这是改写过的拷贝，不是自动同步，核心仓更新之后需要人工搬一次，理由见站点自己的 `README.md`："手册变化频率不高，人工同步的成本低于维护一条跨仓库同步流水线"。

`docs/.vitepress/config.ts` 的 `/guide/` 侧边栏从占位的一条改成真实的五条（概览 + 四个主题），并且现在仓库真的存在了，补上了此前刻意留空的 `editLink`（指向 `mecharion/website` 而不是一个不存在的地址）。首页文案里"使用手册——建设中"改成"备份恢复、证书、升级、故障排查"，如实反映现状；"Pack 仓库——建设中"维持不变，因为这条确实还在等 `pack/v1` 冻结。

**没有做变异验证**：纯文档站点内容与配置，没有产品代码逻辑——与上一条 C7 log 同一个判断。

**验证范围。** 在真实仓库路径下重新 `npm install && npm run docs:build`，构建成功；检查产物确认 `/guide/` 下五个页面（概览+四篇）全部生成；人工核对全部内部链接的产物 `href`——四篇手册之间的互相引用、`guide/index.md` 到各分页、到 GitHub 的绝对链接全部指向真实存在的目标；专门核对了一处带中文锚点的跨页引用（`troubleshooting.md` 链到 `certificates.md#客户端怎么信任这个-ca`）——直接从构建产物里 grep 出 `certificates.html` 里生成的标题 `id`，逐字符核对与链接里的锚点一致，不是凭规则推算。本地 `git commit` 后征询用户是否现在推到远程——确认后 `git push`，`mecharion/website` 的 `main` 分支现在有真实内容。**接 Cloudflare Pages 上线是用户自己在控制台完成的操作**，不在这次改动范围内。

## 2026-08-14（续）：Logo/favicon 上线到三处

**背景。** 用户在仓库之外的本地目录（`D:\Workspace\Develop\GITHUB_CODE\Mecharion\logo\`，不属于任何 git 仓库）完成了 logo 设计——一个 Go 写的零依赖生成器（`generate-logo.go`），从同一套几何参数同时产出 SVG 与位图，此前只导出过 1024px SVG 与一份 256px PNG。征询用户后确定应用范围：Web UI favicon、官网 favicon、README 顶部 logo、官网首页 hero 图标，四项全选。

**先补齐尺寸，再分发。** 用生成器补跑了 32/180/512 三个位图尺寸（`-size`/`-supersample 8` 参数，复用已有工具，没有另外找图像处理库）：32px 给 favicon 位图兜底，180px 是 `apple-touch-icon` 的标准尺寸，512px 留作社交预览图一类用途的备用位图。检查过 32px 版本在缩小后 M 形与琥珀色圆点仍然可辨——这是收进项目前唯一需要人工判断的一步，其余都是机械分发。

**source of truth 放主仓 `docs/brand/`，不是三处各自维护一份「原始」文件。** 新增 `docs/brand/mecharion-logo.svg`（源文件）、`mecharion-logo-512.png`、`README.md`（改写自设计者原来那份 `logo/README.md` 的寓意说明，补一节"用在哪"）。`webui/public/`（`favicon.svg` + `apple-touch-icon.png`）与 `mecharion/website` 仓库的 `docs/public/`（`favicon.svg`/`logo.svg`/`apple-touch-icon.png`）都是从这里**复制**过去的字节，不是引用——两个前端各自独立构建部署，不该在构建期或运行期跨仓依赖对方；`docs/brand/README.md` 明确写了"改动时三处复制都要手工同步"，与运维手册跨仓同步的取舍（人工同步成本低于维护同步流水线）同一条理由。

**三处具体改动。**

- **Web UI**（`webui/`）：`index.html` 新增 `<link rel="icon">`（SVG）与 `<link rel="apple-touch-icon">`；此前完全没有 favicon，浏览器标签页一直是默认图标，是一个真实缺口，不是"更新"而是"从无到有"。
- **官网**（`mecharion/website`）：`docs/public/favicon.svg` 从占位图标（早前随手写的几何图形，用来撑起脚手架）换成真实 logo；新增 `logo.svg`/`apple-touch-icon.png`；`docs/.vitepress/config.ts` 补上 `themeConfig.logo`（导航栏图标，用户没直接点这项，但同一份资源已经在手，零增量成本，与 hero 图标一起加上）与 hero 区域的 `image.src`；`head` 里的 favicon 声明补上显式 `type: 'image/svg+xml'` 与 apple-touch-icon 链接。
- **README.md**：顶部新增居中 logo 图（`docs/brand/mecharion-logo.svg`，GitHub 原生渲染 SVG，不需要转位图）。

`docs/README.md` 补一行"品牌视觉资产，不是设计文档"指向 `docs/brand/README.md`——延续这次会话一路的习惯：新增一个 `docs/` 子目录都要在顶层索引留一笔，不让人翻到才发现。

**没有做变异验证**：纯静态资产与标记语言/配置改动，没有产品代码逻辑可供变异——与 C7 上一条 log 同一个判断。

## 2026-08-14（续）：B1 续——`mechctl --local` 放在名词前面时无法解析（用户真机测试撞出）

**症状。** 用户测试报告：`mechctl component list` 显示"（没有 Component）"（这一步本身没问题，站点确实还没部署任何组件）；但 `mechctl --local component list` 直接报 `错误: unknown command "list" for "mechctl"`，与预期的"连不上本机 mechlet"完全不是同一类错误——说明命令行在 cobra 层面就没解析对，请求根本没发出去。

**根因。** `internal/cli/ctlcmd/component.go` 的 `ClientFlags.Bind()`（`--server`/`--token`/`--ca-file`/`--insecure-skip-verify`/`--site`/`--local`/`--local-socket` 共 7 个 flag）此前由 `component`/`node`/`pack` 等 9 个名词命令各自在自己的 `PersistentFlags()` 上注册一份——这正是 [[B2]] REV-011 给 `-o/--output` 修过的同一个模式，但那次只收编了 `-o`，这 7 个没有跟进。cobra 的 `Command.Find()` 在还没定位到 `component` 子命令之前，用的是**根命令自己的** flag 集合去判断一个 token 是不是待值 flag；`--local` 这时候在根命令上完全不存在，`Find()` 只能按"疑似待值 flag"处理，把紧跟着的 `component` 当成 `--local` 的值吞掉，剩下 `list` 被当成根命令自己的子命令去找，找不到，报出文档字面意义上"命令不存在"的错误。

`10-cli.md §1.5`、`troubleshooting.md`、`01-architecture.md` 等处的规范示例从一开始就写的是 `mechctl --local component status`（flag 在名词前面）——文档教的写法本身就会触发这个 bug。B1（REV-009）开发时手工在容器里验证过这个顺序、当时是通的，但那次核对不严谨；此后新增的自动化测试（`test/e2e/localstatus_linux_test.go` 的 `runLocal`）实际拼的命令是 `component status --local`（flag 在名词后面），从没有测过文档写的那个顺序，这条回归因此一直没被网住，直到这次用户真机测试撞出来。

**关键决定。**

- **仿照 REV-011，物理上只留一处能注册。** 不是给根命令再加一份"影子" `--local`（parentsPflags 合并时会被子命令自己那份同名 flag 悄悄吃掉，两处定义永远只有一处真正生效，迟早再分叉一次）；而是彻底删掉 9 个名词命令各自的 `ClientFlags{Global: gf}` 构造 + `flags.Bind(cmd)`，改成由 `cmd/mechctl/main.go` 造一份共享的 `*ctlcmd.ClientFlags`、只在 root 上 `Bind` 一次，`New*Cmd()` 的签名从接收 `*cli.GlobalFlags` 改成直接接收这个共享的 `*ClientFlags`。`--local` 由"每个名词各自一份独立状态"变成"整个进程一份"，这本来就更符合它的语义——它是"这整条命令走不走 mechd"的开关，不该按名词分裂成互不相干的七八份。
- **不动 `internal/cli/root.go`。** `cli.NewRoot()` 是 mechctl/mechd/mechlet/mechpack 四个二进制共用的骨架，`--local`/`--server` 等是 mechctl 独有的客户端概念，混进共享骨架会让另外三个二进制的 `--help` 里冒出用不上的 flag。`ClientFlags.Bind()` 保留在 `ctlcmd` 包里，只是调用方从"每个 `New*Cmd()` 内部调一次"改成"`main()` 里对 root 调一次"——不需要移动 `DefaultServer`/`DefaultCAFile` 等常量，也不需要给 `cli.GlobalFlags` 加字段，改动面比最初设想的移植整个 `ClientFlags` 到 `internal/cli` 小得多。
- **测试基础设施同步补：独立跑名词命令树的测试必须挂真实 root。** `confirm_test.go`/`userbootstrap_test.go`/`removeflow_test.go`/`localstatus_test.go` 等此前都是直接 `NewXCmd(&cli.GlobalFlags{})` 拿到一棵孤立的子命令树执行——这样测过去是因为 flag 就绑在这棵孤立树自己身上，改造之后它们不会再有任何 client flag，必须像 `wire_test.go` 里 REV-011 已经验证过的做法一样走一棵真实 root。抽了一个共享夹具 `newMechctlRoot()`（`wire_test.go`）给这些测试文件复用，减少了 `wire_test.go` 自己 `run`/`runFull` 原来的重复接线代码。`localstatus_test.go` 的 `runLocal` 顺带改成走文档规范的 `--local` 在名词前的顺序（此前测的是名词后），`test/e2e/localstatus_linux_test.go` 的 `runLocal` 做了同样的调整——这两处此前都没测到文档承诺的那个顺序，这次一起补上，否则同类回归还会再逃过一次。

**实现。** `internal/cli/ctlcmd/{component,apply,backup,config,node,orphans,pack,rollout,user}.go`：9 个 `New*Cmd()` 签名从 `(gf *rootcli.GlobalFlags)` 改成 `(flags *ClientFlags)`，删掉每处内部的 `flags := &ClientFlags{Global: gf}` 与 `flags.Bind(cmd)`。`cmd/mechctl/main.go`：`gf` 之后新增 `flags := &ctlcmd.ClientFlags{Global: gf}; flags.Bind(root)`，9 处 `New*Cmd(gf)` 改成 `New*Cmd(flags)`。测试：`wire_test.go` 新增 `newMechctlRoot()` 夹具，`run`/`runFull` 改用它；`confirm_test.go`/`userbootstrap_test.go`/`removeflow_test.go`/`localstatus_test.go` 同步改用（并删掉各自不再需要的 `internal/cli` 直接 import）；`docdrift_test.go`/`render_test.go` 只是把裸的 `&cli.GlobalFlags{}` 换成 `&ClientFlags{Global: &cli.GlobalFlags{}}`，不需要真 root（这两处不执行命令，只检查命令树结构或不涉及 client flag）。`test/e2e/localstatus_linux_test.go` 的 `runLocal` 调整 flag 顺序。

**变异验证。** 在 `newMechctlRoot()` 里临时去掉 `flags.Bind(root)` 这一行（模拟"根命令不认识这些 flag"的旧状态）：`localstatus_test.go` 的 4 个 `TestLocalStatus*` 里有 3 个按预期变红，报出的错误正是 `unknown command "status" for "mechctl"`——与用户真机撞到的 `unknown command "list" for "mechctl"` 是同一类错误，证明测试的判别力精确落在这条回归本身；第 4 个（`TestLocalStatusRejectsComponentName`，本来就期望任何形式的报错）照原样通过，符合预期不算假阴性。改回后复测为绿。另外手工用编译产物验证过 `mechctl --local component list` / `mechctl component --local list` / `mechctl component list --local` 三种 flag 位置，现在全部一致地解析到"连不上本机 mechlet"这条真实的下一层错误，不再有任何一种在 cobra 解析层面失败。

**测试。** 没有新增独立测试文件——`localstatus_test.go` 的 `runLocal` 与 `test/e2e/localstatus_linux_test.go` 的 `runLocal` 改成文档规范的 flag-在名词-前顺序后，两处原有的用例集本身就成为这条回归的钉子（尤其是容器化的 `TestLocalStatusWorksWhenMechdIsDown`，覆盖了 mechd 可达/不可达两种场景下这个顺序都要能用）。

**验证范围。** `go build/vet/test ./...`（Windows）全绿。容器化验收（`./hack/testenv.sh up && test`，单节点，M2–M9 全量）：除 `test-ctlcmd` 的 `TestDocumentedVerbsExistOrAreMarked`/`TestMarkedVerbsAreReallyMissing` 外全部 `PASS`——这两条是此前已经记录过多次的既有相对路径问题（测试二进制离开 `go test` 的工作目录语境后读不到 `docs/design/10-cli.md`），与本次改动无关，可在任何提交上复现；`test-e2e` 整体 `PASS`，含改过顺序的 `TestLocalStatusWorksWhenMechdIsDown`。

**验证范围。** `webui`：`npm run build` 后检查产物 `dist/index.html` 确认两条 `<link>` 标签与对应文件（`favicon.svg`/`apple-touch-icon.png`）都在 `dist/` 里；`go build/vet/test ./...`（Windows）全绿，确认 `internal/webui` 的 `go:embed` 没有因为新增静态文件而出问题（`all:dist` 本来就会收纳任意新增文件，不需要改 embed 指令）。官网：`npm run docs:build` 后检查产物，导航栏 logo 与 hero 图标的 `<img>` 标签都指向 `/logo.svg` 且该文件确实在产物里；起了一次本地 `docs:preview`（`http://localhost:4173`），确认首页与 favicon 请求都返回 200 后关掉——静态资源路径这一层已经用 HTTP 状态码验证过是真实可达的，DOM 结构本身用的是 VitePress 官方主题组件（`VPImage`），不是自己写的模板，视觉正确性交给这个已经被广泛使用的组件本身负责，没有再截图逐像素核对。这几处改动最初都还没有 commit/push；`mecharion/website` 一侧随后征询用户确认后已经 `git commit` + `git push`（`main` 分支 `1e36f89..b017296`）。`mecharion` 主仓这一侧（`webui/`、`README.md`、`docs/brand/`、`docs/README.md`）仍未提交，与本会话全程的既有状态一致（从未调用过 `git commit`）。

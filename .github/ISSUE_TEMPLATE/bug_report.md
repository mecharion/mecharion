---
name: Bug 报告
about: 代码或文档与实际行为不符
title: ''
labels: bug
assignees: ''
---

**现象**
发生了什么，期望是什么。

**复现步骤**
1.
2.
3.

**环境**
- Mecharion 版本/commit：
- 部署形态：单机 / 多节点
- OS 与 systemd 版本（`mechctl`/`mechpack` 需要三平台，`mechd`/`mechlet` 需要 Linux + systemd）：

**日志/输出**
相关的 `journalctl -u mecharion-mechd`/`mecharion-mechlet`、命令输出或截图。**贴日志前请确认不含口令、token、证书私钥等敏感信息**——参数标了 `sensitive: true` 的值本该在日志里脱敏，但请自行复核。

**是不是设计如此？**
先看一眼 [SECURITY.md](../../SECURITY.md#这个项目的信任边界) 与相关 ADR——有些行为是有意的设计边界，不是缺陷。

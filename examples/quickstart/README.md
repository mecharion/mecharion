# Quickstart 示例

`hello/` 是 [README quickstart](../../README.md) 用的最小示例，**真实可部署**——载荷由
[`hack/quickstartpack.sh`](../../hack/quickstartpack.sh) 现场编译、`mechpack assemble` 算出真实
sha256、`mechpack bundle` 打成 `.mpack`，再用 `mechctl pack upload` 送进 mechd。

**这不是 [`examples/packs/`](../packs/README.md)。** 那边的示例是 pack/v1 规范的验证夹具，
sha256 是 `0000dddd…` 这类占位符，用来压测格式与 lint 规则，装不了——刻意这样，见
[`examples/packs/README.md`](../packs/README.md)。这里只有一个目的：让新用户跟着 README
走一遍，能得到一个真的在跑的服务。

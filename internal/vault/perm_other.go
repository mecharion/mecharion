//go:build !unix

package vault

// permEnforced 为 false：Windows 的 ACL 模型与 Unix 权限位不对应，
// 用 0400 创建的文件读回来是 0444，检查「others 位为 0」永远不成立。
//
// mechd 只在 Linux 上运行（docs/design/25-roadmap.md 的平台矩阵）；
// 此处存在仅为让单元测试在开发机上照常通过。
const permEnforced = false

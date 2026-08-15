//go:build unix

package vault

// permEnforced 表示本平台实现 Unix 权限位，因此「主密钥不得对属主之外可读」
// 这条检查有意义。
const permEnforced = true

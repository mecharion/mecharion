// Package design 把设计文档嵌进二进制，供测试读取。
//
// docdrift_test.go 编译出的测试二进制会被复制进容器运行，容器里没有
// docs/ 目录（testenv.sh 只挂载 bin/ 与 examples/，刻意保持贫瘠）。
// 用 embed 把内容打进二进制本身，不依赖运行时的工作目录或文件系统布局。
package design

import _ "embed"

// CLIDoc 是 10-cli.md 的完整内容。
//
//go:embed 10-cli.md
var CLIDoc string

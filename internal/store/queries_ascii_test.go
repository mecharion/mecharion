package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestQueryFilesAreASCII 钉住「查询文件只能是 ASCII」。
//
// sqlc **按字节偏移剥离注释**。注释里出现多字节字符会让偏移错位，
// 生成出被截断的 SQL——实测产出过 `INSEid`、`GetC`、`ListRo` 这样的标识符。
//
// 恶劣之处在于它**不报错**：`sqlc generate` 退出码是 0，只在 stderr 里
// 混着一堆解析告警，而生成的文件要么缺函数、要么带着坏 SQL。等到运行时
// 才炸，且现场看不出与注释有关。
//
// 本项目其余地方一律用中文注释，因此这条约束极易被无意破坏——由测试守住。
// 每条查询的中文理由写在 Go 侧的仓储层，不写进 .sql。
//
// 注意 schema（migrations/*.sql）**不受影响**，sqlc 对它走另一条解析路径，
// 中文注释照常可用。
func TestQueryFilesAreASCII(t *testing.T) {
	requireSourceTree(t)

	files, err := filepath.Glob(filepath.Join("queries", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("queries/ 下没有 .sql 文件——glob 写错了？")
	}

	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if isASCII(line) {
				continue
			}
			t.Errorf("%s:%d 含非 ASCII 字符，会让 sqlc 生成出坏 SQL:\n  %s\n"+
				"  → 把这段说明搬到 Go 侧的仓储层；分隔线也要用 ASCII 的 `-`",
				f, i+1, strings.TrimSpace(line))
		}
	}
}

func isASCII(s string) bool {
	return utf8.RuneCountInString(s) == len(s)
}

// requireSourceTree 跳过「源码不在手边」的运行环境。
//
// 这两条是**对源码树的静态检查**，不是运行期行为检查。`go test ./...`
// 时 CWD 就是包目录，检查照常生效（开发机与 CI 都覆盖得到）；而在
// test/node 容器里只挂了编译好的二进制、没有源码，此时应当跳过而不是误报。
func requireSourceTree(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("queries"); err != nil {
		t.Skip("源码树不在手边（容器里只有二进制）——这是对源码的静态检查，跳过")
	}
}

// TestGeneratedCodeIsPresent 钉住生成的代码已提交。
//
// 离线环境下构建不能依赖再跑一次 sqlc——生成物必须在仓库里。
func TestGeneratedCodeIsPresent(t *testing.T) {
	requireSourceTree(t)

	for _, f := range []string{
		"sqlcgen/db.go", "sqlcgen/models.go",
		"sqlcgen/querier.go", "sqlcgen/expected.sql.go",
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("缺少生成物 %s：请在 internal/store 下运行 sqlc generate 并提交", f)
		}
	}
}

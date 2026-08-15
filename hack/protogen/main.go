// protogen 由 .proto 生成 Go 代码，**不需要 protoc 二进制**。
//
// 常规做法是装一个 protoc（C++ 写的）再加两个插件。对本项目那是三份额外的
// 平台相关依赖：开发机是 Windows、CI 是 Linux、贡献者什么都有可能——每加一个
// 非 Go 的构建期依赖，「clone 下来就能构建」这条就弱一分。
//
// 这里改用 protocompile（纯 Go 的 proto 编译器）解析出描述符，再把
// CodeGeneratorRequest 喂给两个插件。插件本身是 Go 程序，用 `go tool` 跑，
// 只需要模块缓存。于是整条链路只依赖 Go 工具链。
//
//	go run ./hack/protogen
package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/protoutil"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// protoRoot 是 .proto 的导入根。
const protoRoot = "proto"

// modulePath 是本模块的导入路径，用于把生成物落回仓库内。
const modulePath = "github.com/mecharion/mecharion"

// plugins 是要跑的代码生成插件及其参数。
//
// 用 paths=import 而非 source_relative：前者按 go_package 决定输出位置，
// 于是 .proto 的目录结构（按 proto 包名组织）与 Go 包的位置可以各自合理，
// 不必强行一致。
var plugins = []struct {
	tool string
	opts string
}{
	{"google.golang.org/protobuf/cmd/protoc-gen-go", "paths=import"},
	{"google.golang.org/grpc/cmd/protoc-gen-go-grpc", "paths=import"},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "protogen:", err)
		os.Exit(1)
	}
}

func run() error {
	files, err := findProtos(protoRoot)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("%s 下没有 .proto 文件", protoRoot)
	}

	req, err := buildRequest(files)
	if err != nil {
		return err
	}

	for _, p := range plugins {
		r := proto.Clone(req).(*pluginpb.CodeGeneratorRequest)
		r.Parameter = proto.String(p.opts)
		if err := runPlugin(p.tool, r); err != nil {
			return fmt.Errorf("%s: %w", p.tool, err)
		}
	}
	return nil
}

// findProtos 列出全部 .proto，路径相对 protoRoot 且用斜杠分隔。
func findProtos(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

// buildRequest 把 .proto 编译成插件要的 CodeGeneratorRequest。
func buildRequest(files []string) (*pluginpb.CodeGeneratorRequest, error) {
	comp := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(
			&protocompile.SourceResolver{ImportPaths: []string{protoRoot}},
		),
		// 插件要靠注释生成 Go 文档注释——不保留的话生成的代码里
		// 一行说明都没有，那正是最需要说明的地方
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	compiled, err := comp.Compile(context.Background(), files...)
	if err != nil {
		return nil, err
	}

	// 依赖必须排在被依赖者之前，且每个文件只出现一次
	seen := map[string]bool{}
	var protos []*descriptorpb.FileDescriptorProto
	var add func(f linker.File)
	add = func(f linker.File) {
		path := f.Path()
		if seen[path] {
			return
		}
		seen[path] = true
		imports := f.Imports()
		for i := 0; i < imports.Len(); i++ {
			if dep := f.FindImportByPath(imports.Get(i).Path()); dep != nil {
				add(dep)
			}
		}
		protos = append(protos, protoutil.ProtoFromFileDescriptor(f))
	}
	for _, f := range compiled {
		add(f)
	}

	return &pluginpb.CodeGeneratorRequest{
		FileToGenerate: files,
		ProtoFile:      protos,
		CompilerVersion: &pluginpb.Version{
			Major: proto.Int32(5), Minor: proto.Int32(29), Patch: proto.Int32(0),
		},
	}, nil
}

// runPlugin 把请求喂给插件，并落盘它返回的文件。
func runPlugin(tool string, req *pluginpb.CodeGeneratorRequest) error {
	in, err := proto.Marshal(req)
	if err != nil {
		return err
	}

	cmd := exec.Command("go", "tool", tool)
	cmd.Stdin = bytes.NewReader(in)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return err
	}

	var resp pluginpb.CodeGeneratorResponse
	if err := proto.Unmarshal(out, &resp); err != nil {
		return err
	}
	if e := resp.GetError(); e != "" {
		return fmt.Errorf("%s", e)
	}

	for _, f := range resp.File {
		// paths=import 下插件按 go_package 给出完整导入路径，
		// 去掉模块前缀就是仓库内的位置
		name := strings.TrimPrefix(f.GetName(), modulePath+"/")
		if name == f.GetName() {
			return fmt.Errorf("生成物 %s 不在模块 %s 内，检查 go_package 的写法",
				name, modulePath)
		}
		dst := filepath.FromSlash(name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, []byte(f.GetContent()), 0o644); err != nil {
			return err
		}
		fmt.Println("  生成", dst)
	}
	return nil
}

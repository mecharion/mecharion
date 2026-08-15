// uploadprobe 把一个文件 POST 给 mechd，用于 Pack 上传的验收。
//
// 节点镜像里没有 curl（test/multinode 的包注释说明了为什么不给被测机器
// 装工具），而验收表第 16、17 条要的正是「传上去会怎样」——那需要一次
// 真的 HTTP POST，不是一次连通性检查。
package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
)

func main() {
	token := flag.String("token", "", "Bearer token")
	flag.Parse()
	args := flag.Args()
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "用法: uploadprobe -token T <url> <ca.crt> <file>")
		os.Exit(2)
	}
	url, caFile, path := args[0], args[1], args[2]

	pem, err := os.ReadFile(caFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读 CA:", err)
		os.Exit(1)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)

	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "打开文件:", err)
		os.Exit(1)
	}
	defer f.Close()
	st, _ := f.Stat()

	req, err := http.NewRequest(http.MethodPost, url, f)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if *token != "" {
		req.Header.Set("Authorization", "Bearer "+*token)
	}
	// 显式给长度：让服务端能提前拒绝超限的上传，而不是收完再说
	req.ContentLength = st.Size()

	cli := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	resp, err := cli.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "请求失败:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("HTTP %d  （上传 %d 字节）\n%s\n", resp.StatusCode, st.Size(), string(body))
	if resp.StatusCode >= 300 {
		os.Exit(1)
	}
}

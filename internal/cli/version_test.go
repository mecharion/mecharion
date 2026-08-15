package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mecharion/mecharion/internal/logging"
)

func TestVersionText(t *testing.T) {
	root, _ := NewRoot(Options{Name: "mechctl", Short: "x"})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"mechctl", "commit:", "platform:"} {
		if !strings.Contains(got, want) {
			t.Errorf("输出缺少 %q\n实际:\n%s", want, got)
		}
	}
}

func TestVersionShort(t *testing.T) {
	root, _ := NewRoot(Options{Name: "mechlet", Short: "x"})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"version", "--short"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); !strings.HasPrefix(got, "0.") {
		t.Errorf("--short 应只输出版本号，实际 %q", got)
	}
}

func TestVersionJSON(t *testing.T) {
	root, _ := NewRoot(Options{Name: "mechd", Short: "x"})
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"version", "-o", "json"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, buf.String())
	}
	if got["component"] != "mechd" {
		t.Errorf("component = %q, 期望 mechd", got["component"])
	}
}

func TestInvalidOutputRejected(t *testing.T) {
	root, _ := NewRoot(Options{Name: "mechctl", Short: "x"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"version", "-o", "xml"})

	if err := root.Execute(); err == nil {
		t.Fatal("非法输出格式应当被拒绝")
	}
}

func TestParseLevel(t *testing.T) {
	for _, s := range []string{"debug", "INFO", " warn ", "error", ""} {
		if _, err := logging.ParseLevel(s); err != nil {
			t.Errorf("ParseLevel(%q) 意外失败: %v", s, err)
		}
	}
	if _, err := logging.ParseLevel("trace"); err == nil {
		t.Error("未知级别应当报错")
	}
}

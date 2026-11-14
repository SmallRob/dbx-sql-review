package dbxpluginsdk

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type noopHandler struct{}

func (noopHandler) Handle(RequestContext, string, json.RawMessage, *Emitter) (any, *PluginError) {
	return map[string]any{"ok": true}, nil
}

// initializeOnce 跑一次真实握手，返回 stdout 的第一行响应与解码后的初始化结果。
func initializeOnce(t *testing.T, capabilities []string) (string, map[string]any) {
	t.Helper()
	request := `{"jsonrpc":"2.0","id":1,"method":"plugin/initialize","params":{"host":{"protocolVersions":[1]}}}` + "\n"
	var out bytes.Buffer
	server := NewServer(
		Metadata{ID: "io.github.smallrob.sql-review", Version: "test", Capabilities: capabilities},
		noopHandler{},
	).WithIO(strings.NewReader(request), &out, &bytes.Buffer{})
	if err := server.Serve(); err != nil {
		t.Fatalf("Serve 失败: %v", err)
	}
	line := strings.TrimSpace(out.String())
	if line == "" {
		t.Fatal("sidecar 没有输出任何响应")
	}
	var response struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (原文=%s)", err, line)
	}
	return line, response.Result
}

// 空 capabilities 必须编码成 []，不能是 null：宿主按 sequence 解码该字段，
// null 会让整个插件初始化失败（这正是 v0.1.0 在 DBX 里不可用的原因）。
func TestInitializeEncodingEmptyCapabilities(t *testing.T) {
	raw, result := initializeOnce(t, nil)
	if !strings.Contains(raw, `"capabilities":[]`) {
		t.Fatalf(`capabilities 未编码成空数组，实际响应: %s`, raw)
	}
	if _, ok := result["capabilities"].([]any); !ok {
		t.Fatalf("capabilities 不是数组: %#v", result["capabilities"])
	}
}

// 宿主侧的等价检查：把 capabilities 解到 []string 里不能得到 nil。
func TestInitializeCapabilitiesDecodeAsSequence(t *testing.T) {
	_, result := initializeOnce(t, []string{"connections"})
	blob, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("重新编码失败: %v", err)
	}
	var decoded struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("按序列解码失败: %v", err)
	}
	if len(decoded.Capabilities) != 1 || decoded.Capabilities[0] != "connections" {
		t.Fatalf("capabilities 解码结果异常: %#v", decoded.Capabilities)
	}
}

func TestInitializeRejectsUnknownProtocolVersion(t *testing.T) {
	request := `{"jsonrpc":"2.0","id":1,"method":"plugin/initialize","params":{"host":{"protocolVersions":[99]}}}` + "\n"
	var out bytes.Buffer
	server := NewServer(Metadata{ID: "io.github.smallrob.sql-review", Version: "test"}, noopHandler{}).
		WithIO(strings.NewReader(request), &out, &bytes.Buffer{})
	if err := server.Serve(); err != nil {
		t.Fatalf("Serve 失败: %v", err)
	}
	if !strings.Contains(out.String(), `"code":-32001`) {
		t.Fatalf("期望返回 -32001，实际: %s", strings.TrimSpace(out.String()))
	}
}

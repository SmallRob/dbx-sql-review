// Command dbxpoke 对打包后的 sidecar 二进制做一次真实的 JSON-RPC 握手。
//
// 用途：验证「打包产物里的二进制」确实能按 DBX 插件协议工作，
// 而不是只验证源码能编译。CI 与本地发布前都会跑它。
//
// 用法：go run ./cmd/dbxpoke <path-to-binary>
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: dbxpoke <path-to-binary>")
		os.Exit(2)
	}
	bin := os.Args[1]
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(os.Stderr, "找不到二进制: %v\n", err)
		os.Exit(2)
	}

	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开 stdin 失败: %v\n", err)
		os.Exit(1)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开 stdout 失败: %v\n", err)
		os.Exit(1)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}

	reader := bufio.NewReader(stdout)

	// send 写一条请求并读回一条响应。
	send := func(method string, params any, out any) error {
		req := rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params}
		line, err := json.Marshal(req)
		if err != nil {
			return err
		}
		if _, err := stdin.Write(append(line, '\n')); err != nil {
			return err
		}
		type result struct {
			rpcResponse
		}
		var resp rpcResponse
		deadline := time.Now().Add(30 * time.Second)
		for {
			if time.Now().After(deadline) {
				return fmt.Errorf("等待 %s 响应超时", method)
			}
			text, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("读取 %s 响应失败: %w (stderr=%s)", method, err, stderr.String())
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			if err := json.Unmarshal([]byte(text), &resp); err != nil {
				return fmt.Errorf("解析 %s 响应失败: %w (原文=%s)", method, err, text)
			}
			break
		}
		if resp.Error != nil {
			return fmt.Errorf("%s 返回错误: %s (code=%d)", method, resp.Error.Message, resp.Error.Code)
		}
		if out != nil {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("解析 %s 结果失败: %w", method, err)
			}
		}
		return nil
	}

	// 1) initialize
	var init struct {
		ProtocolVersion int `json:"protocolVersion"`
		Plugin          struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"plugin"`
	}
	if err := send("plugin/initialize", map[string]any{
		"host": map[string]any{"protocolVersions": []int{1}},
	}, &init); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL initialize:", err)
		os.Exit(1)
	}
	fmt.Printf("initialize ok: protocol=%d id=%s version=%s\n",
		init.ProtocolVersion, init.Plugin.ID, init.Plugin.Version)

	// 2) ping
	var ping struct {
		OK     bool   `json:"ok"`
		Ver    string `json:"version"`
		Rules  int    `json:"rules"`
	}
	if err := send("ping", nil, &ping); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL ping:", err)
		os.Exit(1)
	}
	if !ping.OK {
		fmt.Fprintln(os.Stderr, "FAIL ping: ok != true")
		os.Exit(1)
	}
	fmt.Printf("ping ok: version=%s rules=%d\n", ping.Ver, ping.Rules)

	// 3) review/sql —— 核心能力，必须能命中规则
	var review struct {
		Result struct {
			Score    int `json:"score"`
			Level    string `json:"level"`
			Findings []struct {
				RuleID string `json:"ruleId"`
				Line   int    `json:"line"`
			} `json:"findings"`
		} `json:"result"`
	}
	sql := "DROP TABLE users;\nUPDATE orders SET total = 0;"
	if err := send("review/sql", map[string]any{"sql": sql, "file": "inline.sql"}, &review); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL review/sql:", err)
		os.Exit(1)
	}
	if len(review.Result.Findings) == 0 {
		fmt.Fprintln(os.Stderr, "FAIL review/sql: 没有命中任何规则")
		os.Exit(1)
	}
	ids := make([]string, 0, len(review.Result.Findings))
	for _, f := range review.Result.Findings {
		ids = append(ids, f.RuleID)
	}
	fmt.Printf("review/sql ok: score=%d level=%s findings=%v\n",
		review.Result.Score, review.Result.Level, ids)

	// 4) rules/get
	var rules struct {
		Rules []struct {
			ID string `json:"id"`
		} `json:"rules"`
	}
	if err := send("rules/get", nil, &rules); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL rules/get:", err)
		os.Exit(1)
	}
	fmt.Printf("rules/get ok: %d rules\n", len(rules.Rules))

	// 5) mcp/tools
	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := send("mcp/tools", nil, &tools); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL mcp/tools:", err)
		os.Exit(1)
	}
	names := make([]string, 0, len(tools.Tools))
	for _, t := range tools.Tools {
		names = append(names, t.Name)
	}
	fmt.Printf("mcp/tools ok: %v\n", names)

	fmt.Println("PASS 打包产物协议握手全部通过")

	// 关闭 stdin：sidecar 的 Serve() 用 bufio.Scanner 读 stdin，收到 EOF 才返回。
	// 不关闭的话 cmd.Wait() 会永久阻塞，CI 上这一步会一直挂到 job 超时。
	_ = stdin.Close()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "sidecar 退出异常: %v (stderr=%s)\n", err, stderr.String())
			os.Exit(1)
		}
	case <-time.After(15 * time.Second):
		fmt.Fprintln(os.Stderr, "FAIL sidecar 未在 15s 内退出")
		_ = cmd.Process.Kill()
		os.Exit(1)
	}
}

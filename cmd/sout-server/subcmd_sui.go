package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ============================ s-ui API 子命令 ============================
//
// 设计分工：Go 负责 HTTP 请求与 JSON 解析，输出「管道分隔的纯文本」；
// shell 负责用 grep/awk 过滤。这样脚本不再需要 python3 的 urllib/json。

var suiHTTPClient = &http.Client{Timeout: 20 * time.Second}

// suiRequest 向 s-ui apiv2 发请求，返回响应体。
// base 形如 http://127.0.0.1:8443/app/apiv2
func suiRequest(base, token, method, endpoint string, form map[string]string) ([]byte, error) {
	full := strings.TrimRight(base, "/") + "/" + strings.TrimLeft(endpoint, "/")

	var body io.Reader
	if form != nil {
		v := url.Values{}
		for k, val := range form {
			v.Set(k, val)
		}
		body = strings.NewReader(v.Encode())
	}

	req, err := http.NewRequest(method, full, body)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Token", token)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := suiHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return b, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return b, nil
}

func cmdSUI(args []string) int {
	if len(args) < 3 {
		fmt.Fprintln(os.Stderr, "用法: sout-server sui <get|save|inbounds|clients> <base> <token> [...]")
		return 2
	}
	sub, base, token := args[0], args[1], args[2]
	rest := args[3:]

	switch sub {
	case "get":
		// 通用 GET：原样输出响应 JSON
		if len(rest) < 1 {
			fmt.Fprintln(os.Stderr, "用法: sout-server sui get <base> <token> <endpoint>")
			return 2
		}
		b, err := suiRequest(base, token, http.MethodGet, rest[0], nil)
		if err != nil && len(b) == 0 {
			fmt.Fprintln(os.Stderr, "请求失败:", err)
			return 1
		}
		_, _ = os.Stdout.Write(b)
		return 0

	case "save":
		// 通用 POST save：<object> <action> <data>
		if len(rest) < 3 {
			fmt.Fprintln(os.Stderr, "用法: sout-server sui save <base> <token> <object> <action> <data>")
			return 2
		}
		form := map[string]string{"object": rest[0], "action": rest[1], "data": rest[2]}
		b, err := suiRequest(base, token, http.MethodPost, "save", form)
		if err != nil {
			fmt.Fprintln(os.Stderr, "保存失败:", err, strings.TrimSpace(string(b)))
			return 1
		}
		_, _ = os.Stdout.Write(b)
		return 0

	case "inbounds":
		// 输出每行：type|tag|listen_port|tls_id
		b, err := suiRequest(base, token, http.MethodGet, "inbounds", nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "查询 inbounds 失败:", err)
			return 1
		}
		return printInboundsBrief(b)

	case "clients":
		// 输出每行：name|id|...
		b, err := suiRequest(base, token, http.MethodGet, "clients", nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "查询 clients 失败:", err)
			return 1
		}
		return printClientsBrief(b)
	}

	fmt.Fprintf(os.Stderr, "未知 sui 子命令: %s\n", sub)
	return 2
}

// suiRows 从 s-ui 响应里取出对象数组，兼容 obj 为数组或 {inbounds:[...]} 两种形态。
func suiRows(b []byte, key string) []map[string]any {
	var resp struct {
		Success bool `json:"success"`
		Obj     any  `json:"obj"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil
	}
	var rows []map[string]any
	appendFrom := func(v any) {
		arr, ok := v.([]any)
		if !ok {
			return
		}
		for _, it := range arr {
			if m, ok := it.(map[string]any); ok {
				rows = append(rows, m)
			}
		}
	}
	switch t := resp.Obj.(type) {
	case map[string]any:
		if key != "" {
			appendFrom(t[key])
		}
	case []any:
		appendFrom(t)
	}
	return rows
}

func printInboundsBrief(b []byte) int {
	rows := suiRows(b, "inbounds")
	for _, r := range rows {
		typ, _ := r["type"].(string)
		tag, _ := r["tag"].(string)
		port := int(getFloat(r["listen_port"]))
		tlsID := int(getFloat(r["tls_id"]))
		fmt.Printf("%s|%s|%d|%d\n", typ, tag, port, tlsID)
	}
	return 0
}

func printClientsBrief(b []byte) int {
	rows := suiRows(b, "clients")
	for _, r := range rows {
		name, _ := r["name"].(string)
		if name == "" {
			name, _ = r["email"].(string)
		}
		id := int(getFloat(r["id"]))
		fmt.Printf("%s|%d\n", name, id)
	}
	return 0
}

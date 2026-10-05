package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// runSubcommand 处理 sout-server 的子命令。
//
// 这些子命令用于替代 shell 脚本(f.sh / install.sh)里内联的 Python 片段，
// 使脚本层不再依赖 python3。返回 (是否已处理, 退出码)。
//
// 必须在服务初始化(内存防护、日志捕获)之前调用，避免子命令产生服务副作用。
func runSubcommand(args []string) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case "json":
		return true, cmdJSON(args[1:])
	case "sui":
		return true, cmdSUI(args[1:])
	}
	return false, 0
}

// ============================ JSON 子命令 ============================

// loadJSONMap 读取 JSON 对象；文件不存在或内容为空时返回空 map（不报错）。
func loadJSONMap(path string) (map[string]any, error) {
	m := map[string]any{}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// saveJSONMap 以 2 空格缩进写回，并强制 0600（与原 Python 实现一致）。
func saveJSONMap(path string, m map[string]any) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0600); err != nil {
		return err
	}
	_ = os.Chmod(path, 0600)
	return nil
}

// jsonLookup 按点分路径读取，例如 "tls.server_name"。
func jsonLookup(m map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = m
	for _, p := range parts {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// jsonAssign 按点分路径写入，中间层不存在则自动创建。
func jsonAssign(m map[string]any, path string, val any) {
	parts := strings.Split(path, ".")
	cur := m
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = val
			return
		}
		nxt, ok := cur[p].(map[string]any)
		if !ok {
			nxt = map[string]any{}
			cur[p] = nxt
		}
		cur = nxt
	}
}

// jsonDelete 按点分路径删除字段。
func jsonDelete(m map[string]any, path string) bool {
	parts := strings.Split(path, ".")
	cur := m
	for i, p := range parts {
		if i == len(parts)-1 {
			if _, ok := cur[p]; ok {
				delete(cur, p)
				return true
			}
			return false
		}
		nxt, ok := cur[p].(map[string]any)
		if !ok {
			return false
		}
		cur = nxt
	}
	return false
}

// parseJSONValue 把命令行字符串转成合适的 JSON 类型（数字/bool/null/数组/对象/字符串）。
func parseJSONValue(s string) any {
	switch strings.ToLower(s) {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	t := strings.TrimSpace(s)
	if (strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}")) ||
		(strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]")) {
		var v any
		if json.Unmarshal([]byte(t), &v) == nil {
			return v
		}
	}
	return s
}

// formatJSONValue 以 shell 友好的形式输出：字符串原样、bool 小写、数字无多余小数。
func formatJSONValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// shellQuote 单引号包裹，供 eval 使用时不发生二次展开。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func cmdJSON(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: sout-server json <get|set|del|dump|keys> ...")
		return 2
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "get":
		// 单路径：直接输出值（便于 $(...) 捕获）
		// 多路径：输出 KEY=VALUE（便于 eval），键名转为大写并替换 . - 为 _
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "用法: sout-server json get <file> <path> [more...]")
			return 2
		}
		m, err := loadJSONMap(rest[0])
		if err != nil {
			return 1
		}
		if len(rest) == 2 {
			v, _ := jsonLookup(m, rest[1])
			fmt.Println(formatJSONValue(v))
			return 0
		}
		for _, p := range rest[1:] {
			v, _ := jsonLookup(m, p)
			key := strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(p))
			fmt.Printf("%s=%s\n", key, shellQuote(formatJSONValue(v)))
		}
		return 0

	case "set":
		// 用法: json set <file> <path>=<value> [<path>=<value> ...]
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "用法: sout-server json set <file> <path>=<value> ...")
			return 2
		}
		m, err := loadJSONMap(rest[0])
		if err != nil {
			return 1
		}
		for _, kv := range rest[1:] {
			i := strings.Index(kv, "=")
			if i <= 0 {
				fmt.Fprintf(os.Stderr, "参数格式错误(应为 path=value): %s\n", kv)
				return 2
			}
			jsonAssign(m, kv[:i], parseJSONValue(kv[i+1:]))
		}
		if err := saveJSONMap(rest[0], m); err != nil {
			fmt.Fprintln(os.Stderr, "写入失败:", err)
			return 1
		}
		return 0

	case "del":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "用法: sout-server json del <file> <path> ...")
			return 2
		}
		m, err := loadJSONMap(rest[0])
		if err != nil {
			return 1
		}
		for _, p := range rest[1:] {
			jsonDelete(m, p)
		}
		if err := saveJSONMap(rest[0], m); err != nil {
			return 1
		}
		return 0

	case "dump":
		if len(rest) < 1 {
			return 2
		}
		m, err := loadJSONMap(rest[0])
		if err != nil {
			return 1
		}
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return 1
		}
		fmt.Println(string(b))
		return 0

	case "keys":
		if len(rest) < 1 {
			return 2
		}
		m, err := loadJSONMap(rest[0])
		if err != nil {
			return 1
		}
		ks := make([]string, 0, len(m))
		for k := range m {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			fmt.Println(k)
		}
		return 0
	}

	fmt.Fprintf(os.Stderr, "未知 json 子命令: %s\n", sub)
	return 2
}

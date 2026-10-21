package provider

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

type yamlLine struct {
	indent int
	text   string
}

type yamlContext struct {
	indent int
	value  map[string]any
}

func parseYAMLSubscriptionPayload(providerID string, body []byte) (ParsedSubscription, error) {
	lines := normalizedYAMLLines(string(body))
	start := -1
	baseIndent := 0
	for i, line := range lines {
		if line.text == "proxies:" {
			start = i + 1
			baseIndent = line.indent
			break
		}
	}
	if start < 0 {
		return ParsedSubscription{}, fmt.Errorf("Clash YAML does not contain proxies")
	}

	var proxies []map[string]any
	var current map[string]any
	itemIndent := -1
	var stack []yamlContext

	flush := func() {
		if current != nil {
			proxies = append(proxies, current)
		}
		current = nil
		stack = nil
		itemIndent = -1
	}

	for _, line := range lines[start:] {
		if line.indent <= baseIndent && !strings.HasPrefix(line.text, "- ") {
			break
		}
		if strings.HasPrefix(line.text, "- ") {
			flush()
			itemIndent = line.indent
			current = map[string]any{}
			stack = []yamlContext{{indent: itemIndent, value: current}}
			rest := strings.TrimSpace(strings.TrimPrefix(line.text, "- "))
			if rest == "" {
				continue
			}
			if strings.HasPrefix(rest, "{") && strings.HasSuffix(rest, "}") {
				copyMap(current, parseInlineMap(rest))
				continue
			}
			key, value, ok := splitYAMLPair(rest)
			if ok {
				current[key] = parseYAMLScalar(value)
			}
			continue
		}
		if current == nil || line.indent <= itemIndent {
			continue
		}
		key, value, ok := splitYAMLPair(line.text)
		if !ok {
			continue
		}
		for len(stack) > 1 && line.indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1].value
		if strings.TrimSpace(value) == "" {
			child := map[string]any{}
			parent[key] = child
			stack = append(stack, yamlContext{indent: line.indent, value: child})
			continue
		}
		parent[key] = parseYAMLScalar(value)
	}
	flush()

	outbounds := make([]map[string]any, 0, len(proxies))
	var errs []string
	for _, proxy := range proxies {
		ob, err := ecosystemProxyToRawNode(proxy)
		if err != nil {
			if len(errs) < 3 {
				errs = append(errs, err.Error())
			}
			continue
		}
		outbounds = append(outbounds, ob)
	}
	if len(outbounds) == 0 {
		if len(errs) > 0 {
			return ParsedSubscription{}, fmt.Errorf("Clash YAML contains no supported proxies: %s", strings.Join(errs, "; "))
		}
		return ParsedSubscription{}, fmt.Errorf("Clash YAML contains no supported proxies")
	}
	return admittedIdentitySources(providerID, outbounds)
}

func normalizedYAMLLines(text string) []yamlLine {
	var lines []yamlLine
	for _, raw := range strings.Split(text, "\n") {
		raw = strings.TrimRight(raw, "\r\t ")
		if strings.TrimSpace(raw) == "" {
			continue
		}
		trimmed := strings.TrimSpace(stripYAMLComment(raw))
		if trimmed == "" || trimmed == "---" || trimmed == "..." {
			continue
		}
		lines = append(lines, yamlLine{indent: leadingSpaces(raw), text: trimmed})
	}
	return lines
}

func stripYAMLComment(raw string) string {
	var quote rune
	escaped := false
	for i, r := range raw {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quote != 0 {
			escaped = true
			continue
		}
		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
			} else if quote == r {
				quote = 0
			}
			continue
		}
		if r == '#' && quote == 0 && (i == 0 || raw[i-1] == ' ' || raw[i-1] == '\t') {
			return raw[:i]
		}
	}
	return raw
}

func leadingSpaces(raw string) int {
	n := 0
	for _, r := range raw {
		if r != ' ' {
			break
		}
		n++
	}
	return n
}

func splitYAMLPair(text string) (string, string, bool) {
	idx := indexUnquoted(text, ':')
	if idx < 0 {
		return "", "", false
	}
	key := strings.TrimSpace(text[:idx])
	value := strings.TrimSpace(text[idx+1:])
	key = strings.Trim(key, `"'`)
	if key == "" {
		return "", "", false
	}
	return key, value, true
}

func indexUnquoted(text string, target rune) int {
	var quote rune
	escaped := false
	for i, r := range text {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quote != 0 {
			escaped = true
			continue
		}
		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
			} else if quote == r {
				quote = 0
			}
			continue
		}
		if r == target && quote == 0 {
			return i
		}
	}
	return -1
}

func parseYAMLScalar(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}") {
		return parseInlineMap(value)
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		return parseInlineList(value)
	}
	unquoted := unquoteYAML(value)
	switch strings.ToLower(unquoted) {
	case "true":
		return true
	case "false":
		return false
	case "null", "~":
		return ""
	}
	if i, err := strconv.Atoi(unquoted); err == nil {
		return i
	}
	return unquoted
}

func parseInlineMap(value string) map[string]any {
	out := map[string]any{}
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(value), "{"), "}"))
	for _, part := range splitInlineYAML(inner) {
		key, val, ok := splitYAMLPair(part)
		if ok {
			out[key] = parseYAMLScalar(val)
		}
	}
	return out
}

func parseInlineList(value string) []string {
	inner := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(value), "["), "]"))
	var out []string
	for _, part := range splitInlineYAML(inner) {
		if s := strings.TrimSpace(unquoteYAML(part)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func splitInlineYAML(text string) []string {
	var out []string
	var quote rune
	escaped := false
	depth := 0
	start := 0
	for i, r := range text {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quote != 0 {
			escaped = true
			continue
		}
		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
			} else if quote == r {
				quote = 0
			}
			continue
		}
		if quote == 0 {
			switch r {
			case '{', '[':
				depth++
			case '}', ']':
				if depth > 0 {
					depth--
				}
			case ',':
				if depth == 0 {
					out = append(out, strings.TrimSpace(text[start:i]))
					start = i + 1
				}
			}
		}
	}
	tail := strings.TrimSpace(text[start:])
	if tail != "" {
		out = append(out, tail)
	}
	return out
}

func unquoteYAML(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return strings.ReplaceAll(value[1:len(value)-1], `\"`, `"`)
		}
	}
	return value
}

func copyMap(dst, src map[string]any) {
	for k, v := range src {
		dst[k] = v
	}
}

func ecosystemProxyToRawNode(proxy map[string]any) (map[string]any, error) {
	typ := strings.ToLower(firstClashString(proxy, "type"))
	if typ == "hy2" {
		typ = "hysteria2"
	}
	tag := firstClashString(proxy, "name", "tag")
	server := firstClashString(proxy, "server")
	port := firstClashInt(proxy, "port", "server_port")
	if tag == "" || server == "" || port <= 0 {
		return nil, fmt.Errorf("proxy %q missing name/server/port", tag)
	}

	ob := map[string]any{
		"tag":         tag,
		"server":      server,
		"server_port": port,
	}
	switch typ {
	case "ss", "shadowsocks":
		ob["type"] = "shadowsocks"
		ob["method"] = firstClashString(proxy, "cipher", "method")
		ob["password"] = firstClashString(proxy, "password")
	case "vmess":
		ob["type"] = "vmess"
		ob["uuid"] = firstClashString(proxy, "uuid")
		if security := firstClashString(proxy, "cipher", "security"); security != "" && security != "auto" {
			ob["security"] = security
		}
		if alterID := firstClashInt(proxy, "alterId", "alter-id", "alter_id"); alterID > 0 {
			ob["alter_id"] = alterID
		}
		addClashTLS(ob, proxy, false)
		addClashTransport(ob, proxy)
	case "vless":
		ob["type"] = "vless"
		ob["uuid"] = firstClashString(proxy, "uuid")
		if flow := firstClashString(proxy, "flow"); flow != "" {
			ob["flow"] = flow
		}
		addClashTLS(ob, proxy, isRealityProxy(proxy))
		addClashTransport(ob, proxy)
	case "trojan":
		ob["type"] = "trojan"
		ob["password"] = firstClashString(proxy, "password")
		addClashTLS(ob, proxy, false)
	case "anytls":
		ob["type"] = "anytls"
		ob["password"] = firstClashString(proxy, "password")
		addClashTLS(ob, proxy, false)
	case "hysteria2":
		ob["type"] = "hysteria2"
		ob["password"] = firstClashString(proxy, "password")
		if up := firstClashInt(proxy, "up", "upmbps", "up-mbps"); up > 0 {
			ob["up_mbps"] = up
		}
		if down := firstClashInt(proxy, "down", "downmbps", "down-mbps"); down > 0 {
			ob["down_mbps"] = down
		}
		if obfs := clashObfs(proxy); obfs != nil {
			ob["obfs"] = obfs
		}
		addClashTLS(ob, proxy, false)
	case "hysteria":
		ob["type"] = "hysteria"
		if auth := firstClashString(proxy, "auth-str", "auth_str", "auth"); auth != "" {
			ob["auth_str"] = auth
		}
		if up := firstClashInt(proxy, "up", "upmbps", "up-mbps"); up > 0 {
			ob["up_mbps"] = up
		}
		if down := firstClashInt(proxy, "down", "downmbps", "down-mbps"); down > 0 {
			ob["down_mbps"] = down
		}
		if obfs := firstClashString(proxy, "obfs"); obfs != "" {
			ob["obfs"] = obfs
		}
		addClashTLS(ob, proxy, false)
	case "tuic":
		ob["type"] = "tuic"
		ob["uuid"] = firstClashString(proxy, "uuid")
		ob["password"] = firstClashString(proxy, "password")
		if cc := firstClashString(proxy, "congestion-controller", "congestion_control"); cc != "" {
			ob["congestion_control"] = cc
		}
		addClashTLS(ob, proxy, false)
	case "socks5", "socks":
		ob["type"] = "socks"
		if username := firstClashString(proxy, "username"); username != "" {
			ob["username"] = username
		}
		if password := firstClashString(proxy, "password"); password != "" {
			ob["password"] = password
		}
	case "http":
		ob["type"] = "http"
		if username := firstClashString(proxy, "username"); username != "" {
			ob["username"] = username
		}
		if password := firstClashString(proxy, "password"); password != "" {
			ob["password"] = password
		}
	default:
		return nil, fmt.Errorf("unsupported Clash proxy type %q", typ)
	}
	return ob, nil
}

func addClashTLS(ob map[string]any, proxy map[string]any, reality bool) {
	enabled := clashBool(proxy, "tls") || reality ||
		firstClashString(proxy, "sni", "servername", "server_name") != "" ||
		clashBool(proxy, "skip-cert-verify", "skip_cert_verify") ||
		firstClashString(proxy, "client-fingerprint", "client_fingerprint") != ""
	if !enabled {
		return
	}
	tls := map[string]any{"enabled": true}
	if serverName := firstClashString(proxy, "sni", "servername", "server_name"); serverName != "" {
		tls["server_name"] = serverName
	}
	if clashBool(proxy, "skip-cert-verify", "skip_cert_verify") {
		tls["insecure"] = true
	}
	if fp := firstClashString(proxy, "client-fingerprint", "client_fingerprint"); fp != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	if alpn := clashStringList(proxy, "alpn"); len(alpn) > 0 {
		tls["alpn"] = alpn
	}
	if reality {
		realityConfig := map[string]any{"enabled": true}
		if opts, ok := clashMap(proxy, "reality-opts", "reality_opts"); ok {
			if publicKey := firstClashString(opts, "public-key", "public_key"); publicKey != "" {
				realityConfig["public_key"] = publicKey
			}
			if shortID := firstClashString(opts, "short-id", "short_id"); shortID != "" {
				realityConfig["short_id"] = shortID
			}
		}
		if _, ok := tls["utls"]; !ok {
			tls["utls"] = map[string]any{"enabled": true, "fingerprint": "chrome"}
		}
		tls["reality"] = realityConfig
	}
	ob["tls"] = tls
}

func addClashTransport(ob map[string]any, proxy map[string]any) {
	switch strings.ToLower(firstClashString(proxy, "network")) {
	case "ws", "websocket":
		transport := map[string]any{"type": "ws"}
		if path := firstClashString(proxy, "ws-path", "ws_path"); path != "" {
			transport["path"] = path
		}
		if opts, ok := clashMap(proxy, "ws-opts", "ws_opts"); ok {
			if path := firstClashString(opts, "path"); path != "" {
				transport["path"] = path
			}
			if headers, ok := clashMap(opts, "headers"); ok {
				if host := firstClashString(headers, "Host", "host"); host != "" {
					transport["headers"] = map[string]any{"Host": host}
				}
			} else if host := firstClashString(opts, "host", "Host"); host != "" {
				transport["headers"] = map[string]any{"Host": host}
			}
		}
		ob["transport"] = transport
	case "grpc":
		transport := map[string]any{"type": "grpc"}
		if opts, ok := clashMap(proxy, "grpc-opts", "grpc_opts"); ok {
			if service := firstClashString(opts, "grpc-service-name", "grpc_service_name", "serviceName", "service_name"); service != "" {
				transport["service_name"] = service
			}
		}
		ob["transport"] = transport
	}
}

func isRealityProxy(proxy map[string]any) bool {
	if strings.EqualFold(firstClashString(proxy, "security"), "reality") {
		return true
	}
	_, ok := clashMap(proxy, "reality-opts", "reality_opts")
	return ok
}

func clashObfs(proxy map[string]any) map[string]any {
	obfsType := firstClashString(proxy, "obfs")
	password := firstClashString(proxy, "obfs-password", "obfs_password")
	if obfsType == "" && password == "" {
		return nil
	}
	obfs := map[string]any{}
	if obfsType != "" {
		obfs["type"] = obfsType
	}
	if password != "" {
		obfs["password"] = password
	}
	return obfs
}

func firstClashString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := lookupClashValue(values, key); ok {
			switch x := v.(type) {
			case string:
				return strings.TrimSpace(x)
			case fmt.Stringer:
				return strings.TrimSpace(x.String())
			case int:
				return strconv.Itoa(x)
			case int64:
				return strconv.FormatInt(x, 10)
			case float64:
				return strconv.FormatFloat(x, 'f', -1, 64)
			case bool:
				if x {
					return "true"
				}
				return "false"
			}
		}
	}
	return ""
}

func firstClashInt(values map[string]any, keys ...string) int {
	for _, key := range keys {
		if v, ok := lookupClashValue(values, key); ok {
			switch x := v.(type) {
			case int:
				return x
			case int64:
				return int(x)
			case float64:
				return int(x)
			case string:
				n, _ := strconv.Atoi(strings.TrimSpace(x))
				return n
			}
		}
	}
	if hostport := firstClashString(values, "server"); strings.Contains(hostport, ":") {
		if _, p, err := net.SplitHostPort(hostport); err == nil {
			n, _ := strconv.Atoi(p)
			return n
		}
	}
	return 0
}

func clashBool(values map[string]any, keys ...string) bool {
	for _, key := range keys {
		if v, ok := lookupClashValue(values, key); ok {
			switch x := v.(type) {
			case bool:
				return x
			case string:
				switch strings.ToLower(strings.TrimSpace(x)) {
				case "true", "1", "yes":
					return true
				}
			case int:
				return x != 0
			}
		}
	}
	return false
}

func clashMap(values map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if v, ok := lookupClashValue(values, key); ok {
			if m, ok := v.(map[string]any); ok {
				return m, true
			}
		}
	}
	return nil, false
}

func clashStringList(values map[string]any, keys ...string) []string {
	for _, key := range keys {
		if v, ok := lookupClashValue(values, key); ok {
			switch x := v.(type) {
			case []string:
				return x
			case []any:
				var out []string
				for _, item := range x {
					if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
						out = append(out, s)
					}
				}
				return out
			case string:
				if strings.Contains(x, ",") {
					return strings.Split(x, ",")
				}
				if strings.TrimSpace(x) != "" {
					return []string{strings.TrimSpace(x)}
				}
			}
		}
	}
	return nil
}

func lookupClashValue(values map[string]any, key string) (any, bool) {
	if v, ok := values[key]; ok {
		return v, true
	}
	normalized := normalizeClashKey(key)
	for k, v := range values {
		if normalizeClashKey(k) == normalized {
			return v, true
		}
	}
	return nil, false
}

func normalizeClashKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "_", "-")
	return key
}

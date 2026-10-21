package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

func parseURIListPayload(providerID string, body []byte) (ParsedSubscription, error) {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return ParsedSubscription{}, fmt.Errorf("URI subscription body is empty")
	}
	if !strings.Contains(text, "://") {
		if decoded, ok := decodeBase64Text(text); ok {
			text = decoded
		}
	}
	var outbounds []map[string]any
	var errs []string
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ob, err := parseNodeURI(line)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		outbounds = append(outbounds, ob)
	}
	if len(outbounds) == 0 {
		if len(errs) > 0 {
			return ParsedSubscription{}, fmt.Errorf("subscription contains no supported URI nodes: %s", strings.Join(errs[:min(len(errs), 3)], "; "))
		}
		return ParsedSubscription{}, fmt.Errorf("subscription contains no supported URI nodes")
	}
	return admittedIdentitySources(providerID, outbounds)
}

func decodeBase64Text(text string) (string, bool) {
	decoded, ok := decodeBase64Any(text)
	if ok && strings.Contains(decoded, "://") {
		return decoded, true
	}
	return "", false
}

func decodeBase64Any(text string) (string, bool) {
	compact := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		default:
			return r
		}
	}, text)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		decoded, err := enc.DecodeString(compact)
		if err == nil {
			return string(decoded), true
		}
	}
	return "", false
}

func parseNodeURI(raw string) (map[string]any, error) {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd <= 0 {
		return nil, fmt.Errorf("node URI missing scheme")
	}
	scheme := strings.ToLower(raw[:schemeEnd])
	if scheme == "hy2" {
		scheme = "hysteria2"
	}
	switch scheme {
	case "vmess":
		return parseVMessURI(raw)
	case "ss":
		return parseShadowsocksURI(raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	switch scheme {
	case "anytls":
		return parseAnyTLSURI(u)
	case "vless":
		return parseVLESSURI(u)
	case "trojan":
		return parseTrojanURI(u)
	case "hysteria2":
		return parseHysteria2URI(u)
	default:
		return nil, fmt.Errorf("unsupported URI scheme %q", scheme)
	}
}

func parseAnyTLSURI(u *url.URL) (map[string]any, error) {
	ob, err := baseUserHostOutbound(u, "anytls")
	if err != nil {
		return nil, err
	}
	ob["password"] = u.User.Username()
	ob["tls"] = tlsFromQuery(u.Query(), false)
	return ob, nil
}

func parseVLESSURI(u *url.URL) (map[string]any, error) {
	ob, err := baseUserHostOutbound(u, "vless")
	if err != nil {
		return nil, err
	}
	ob["uuid"] = u.User.Username()
	q := u.Query()
	if flow := q.Get("flow"); flow != "" {
		ob["flow"] = flow
	}
	if q.Get("security") != "none" {
		ob["tls"] = tlsFromQuery(q, q.Get("security") == "reality")
	}
	addTransport(ob, q)
	return ob, nil
}

func parseTrojanURI(u *url.URL) (map[string]any, error) {
	ob, err := baseUserHostOutbound(u, "trojan")
	if err != nil {
		return nil, err
	}
	ob["password"] = u.User.Username()
	ob["tls"] = tlsFromQuery(u.Query(), false)
	addTransport(ob, u.Query())
	return ob, nil
}

func parseHysteria2URI(u *url.URL) (map[string]any, error) {
	ob, err := baseUserHostOutbound(u, "hysteria2")
	if err != nil {
		return nil, err
	}
	q := u.Query()
	ob["password"] = u.User.Username()
	ob["tls"] = tlsFromQuery(q, false)
	if up := firstNonEmpty(q.Get("upmbps"), q.Get("up")); up != "" {
		if v, err := strconv.Atoi(up); err == nil {
			ob["up_mbps"] = v
		}
	}
	if down := firstNonEmpty(q.Get("downmbps"), q.Get("down")); down != "" {
		if v, err := strconv.Atoi(down); err == nil {
			ob["down_mbps"] = v
		}
	}
	if typ := q.Get("obfs"); typ != "" {
		ob["obfs"] = map[string]any{
			"type":     typ,
			"password": firstNonEmpty(q.Get("obfs-password"), q.Get("obfs_password")),
		}
	}
	return ob, nil
}

func parseVMessURI(raw string) (map[string]any, error) {
	payload := strings.TrimPrefix(raw, "vmess://")
	decoded, ok := decodeBase64Any(payload)
	if !ok {
		return nil, fmt.Errorf("invalid vmess base64 payload")
	}
	var vm map[string]any
	if err := json.Unmarshal([]byte(decoded), &vm); err != nil {
		return nil, err
	}
	server := strings.TrimSpace(fmt.Sprint(vm["add"]))
	port, _ := strconv.Atoi(strings.TrimSpace(fmt.Sprint(vm["port"])))
	tag := strings.TrimSpace(fmt.Sprint(vm["ps"]))
	if tag == "" {
		tag = server
	}
	ob := map[string]any{
		"type":        "vmess",
		"tag":         tag,
		"server":      server,
		"server_port": port,
		"uuid":        strings.TrimSpace(fmt.Sprint(vm["id"])),
	}
	if security := strings.TrimSpace(fmt.Sprint(vm["scy"])); security != "" && security != "<nil>" {
		ob["security"] = security
	}
	if strings.EqualFold(strings.TrimSpace(fmt.Sprint(vm["tls"])), "tls") {
		tls := map[string]any{"enabled": true}
		if sni := strings.TrimSpace(fmt.Sprint(vm["sni"])); sni != "" && sni != "<nil>" {
			tls["server_name"] = sni
		}
		ob["tls"] = tls
	}
	return ob, nil
}

func parseShadowsocksURI(raw string) (map[string]any, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	tag, _ := url.QueryUnescape(u.Fragment)
	if tag == "" {
		tag = u.Hostname()
	}
	var method, password string
	if u.User != nil {
		methodAndPassword, ok := decodeBase64Any(u.User.Username())
		if !ok {
			methodAndPassword = u.User.Username()
		}
		method, password, _ = strings.Cut(methodAndPassword, ":")
	} else {
		decoded, ok := decodeBase64Any(strings.TrimPrefix(raw, "ss://"))
		if !ok {
			return nil, fmt.Errorf("invalid shadowsocks payload")
		}
		beforeFragment, _, _ := strings.Cut(decoded, "#")
		credentials, hostport, ok := strings.Cut(beforeFragment, "@")
		if !ok {
			return nil, fmt.Errorf("invalid shadowsocks decoded payload")
		}
		method, password, _ = strings.Cut(credentials, ":")
		host, port, err := splitHostPort(hostport)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "shadowsocks", "tag": tag, "server": host, "server_port": port, "method": method, "password": password}, nil
	}
	host, port, err := splitHostPort(u.Host)
	if err != nil {
		return nil, err
	}
	return map[string]any{"type": "shadowsocks", "tag": tag, "server": host, "server_port": port, "method": method, "password": password}, nil
}

func baseUserHostOutbound(u *url.URL, typ string) (map[string]any, error) {
	host, port, err := splitHostPort(u.Host)
	if err != nil {
		return nil, err
	}
	tag, _ := url.QueryUnescape(u.Fragment)
	if tag == "" {
		tag = host
	}
	if u.User.Username() == "" {
		return nil, fmt.Errorf("%s URI missing credential", typ)
	}
	return map[string]any{"type": typ, "tag": tag, "server": host, "server_port": port}, nil
}

func splitHostPort(hostport string) (string, int, error) {
	host := strings.TrimSpace(hostport)
	portText := ""
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		host, portText = h, p
	} else {
		lastColon := strings.LastIndex(hostport, ":")
		if lastColon < 0 {
			return "", 0, fmt.Errorf("missing port in %q", hostport)
		}
		host, portText = hostport[:lastColon], hostport[lastColon+1:]
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 {
		return "", 0, fmt.Errorf("invalid port %q", portText)
	}
	if strings.Trim(host, "[]") == "" {
		return "", 0, fmt.Errorf("host is required")
	}
	return strings.Trim(host, "[]"), port, nil
}

func tlsFromQuery(q url.Values, reality bool) map[string]any {
	tls := map[string]any{"enabled": true}
	if sni := firstNonEmpty(q.Get("sni"), q.Get("peer"), q.Get("host")); sni != "" {
		tls["server_name"] = sni
	}
	if truthy(q.Get("insecure")) || truthy(q.Get("allowInsecure")) {
		tls["insecure"] = true
	}
	if fp := firstNonEmpty(q.Get("fp"), q.Get("fingerprint")); fp != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fp}
	}
	if alpn := q.Get("alpn"); alpn != "" {
		tls["alpn"] = strings.Split(alpn, ",")
	}
	if reality {
		if _, ok := tls["utls"]; !ok {
			tls["utls"] = map[string]any{"enabled": true, "fingerprint": "chrome"}
		}
		realityConfig := map[string]any{"enabled": true}
		if pbk := firstNonEmpty(q.Get("pbk"), q.Get("public_key")); pbk != "" {
			realityConfig["public_key"] = pbk
		}
		if sid := firstNonEmpty(q.Get("sid"), q.Get("short_id")); sid != "" {
			realityConfig["short_id"] = sid
		}
		tls["reality"] = realityConfig
	}
	return tls
}

func addTransport(ob map[string]any, q url.Values) {
	switch q.Get("type") {
	case "ws":
		transport := map[string]any{"type": "ws"}
		if path := q.Get("path"); path != "" {
			transport["path"] = path
		}
		if host := q.Get("host"); host != "" {
			transport["headers"] = map[string]any{"Host": host}
		}
		ob["transport"] = transport
	case "grpc":
		transport := map[string]any{"type": "grpc"}
		if service := q.Get("serviceName"); service != "" {
			transport["service_name"] = service
		}
		ob["transport"] = transport
	}
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

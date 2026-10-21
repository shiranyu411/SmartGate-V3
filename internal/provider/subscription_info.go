package provider

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"smartgate/internal/state"
)

const subscriptionUserinfoHeader = "Subscription-Userinfo"

type SubscriptionInfo = state.SubscriptionInfo

var trafficAmountRE = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*([KMGT]?B|[KMGT])`)

func ParseSubscriptionUserinfoHeader(header http.Header, now time.Time) (*SubscriptionInfo, error) {
	raw := strings.TrimSpace(header.Get(subscriptionUserinfoHeader))
	if raw == "" {
		return nil, nil
	}
	return ParseSubscriptionUserinfo(raw, now)
}

func ParseSubscriptionUserinfo(raw string, now time.Time) (*SubscriptionInfo, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	values := map[string]int64{}
	for _, part := range strings.Split(raw, ";") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.Trim(strings.TrimSpace(value), `"`)
		if key == "" || value == "" {
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid Subscription-Userinfo %s value %q", key, value)
		}
		values[key] = n
	}
	upload, hasUpload := values["upload"]
	download, hasDownload := values["download"]
	total, hasTotal := values["total"]
	if !hasUpload && !hasDownload && !hasTotal {
		return nil, fmt.Errorf("Subscription-Userinfo does not contain upload/download/total")
	}
	used := upload + download
	remaining := int64(0)
	if total > used {
		remaining = total - used
	}
	info := SubscriptionInfo{
		UploadBytes:    upload,
		DownloadBytes:  download,
		UsedBytes:      used,
		TotalBytes:     total,
		RemainingBytes: remaining,
		UpdatedAt:      now.UTC(),
		Raw:            raw,
	}
	if expire := values["expire"]; expire > 0 {
		info.ExpireUnix = expire
		expireAt := time.Unix(expire, 0).UTC()
		info.ExpireAt = &expireAt
	}
	return &info, nil
}

func ParseSubscriptionInfoFromOutbounds(outbounds []map[string]any, now time.Time) *SubscriptionInfo {
	var remaining int64
	var expireUnix int64
	var expireAt *time.Time
	rawParts := []string{}
	for _, outbound := range outbounds {
		tag, _ := outbound["tag"].(string)
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if strings.Contains(tag, "剩余流量") {
			if n, ok := parseTrafficAmount(tag); ok {
				remaining = n
				rawParts = append(rawParts, tag)
			}
		}
		if strings.Contains(tag, "套餐到期") || strings.Contains(tag, "到期") {
			if t, ok := parseExpireDate(tag); ok {
				expireUnix = t.Unix()
				expireAt = &t
				rawParts = append(rawParts, tag)
			}
		}
	}
	if remaining <= 0 && expireAt == nil {
		return nil
	}
	info := SubscriptionInfo{
		TotalBytes:     remaining,
		RemainingBytes: remaining,
		UpdatedAt:      now.UTC(),
		Raw:            strings.Join(rawParts, "; "),
	}
	if expireAt != nil {
		info.ExpireUnix = expireUnix
		info.ExpireAt = expireAt
	}
	return &info
}

func parseTrafficAmount(s string) (int64, bool) {
	matches := trafficAmountRE.FindStringSubmatch(s)
	if len(matches) != 3 {
		return 0, false
	}
	n, err := strconv.ParseFloat(matches[1], 64)
	if err != nil || n < 0 {
		return 0, false
	}
	unit := strings.ToUpper(matches[2])
	unit = strings.TrimSuffix(unit, "B")
	multiplier := float64(1)
	switch unit {
	case "K":
		multiplier = 1024
	case "M":
		multiplier = 1024 * 1024
	case "G":
		multiplier = 1024 * 1024 * 1024
	case "T":
		multiplier = 1024 * 1024 * 1024 * 1024
	}
	return int64(math.Round(n * multiplier)), true
}

func parseExpireDate(s string) (time.Time, bool) {
	for _, field := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r == '-')
	}) {
		if len(field) != len("2006-01-02") {
			continue
		}
		t, err := time.Parse("2006-01-02", field)
		if err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

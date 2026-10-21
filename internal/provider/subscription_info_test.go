package provider

import (
	"math"
	"net/http"
	"testing"
	"time"
)

func TestParseSubscriptionUserinfo(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	info, err := ParseSubscriptionUserinfo(`upload=1024; download=2048; total=8192; expire=1799999999`, now)
	if err != nil {
		t.Fatalf("parse subscription userinfo: %v", err)
	}
	if info.UploadBytes != 1024 || info.DownloadBytes != 2048 || info.UsedBytes != 3072 || info.TotalBytes != 8192 || info.RemainingBytes != 5120 {
		t.Fatalf("unexpected traffic info: %+v", info)
	}
	if info.ExpireUnix != 1799999999 || info.ExpireAt == nil || info.UpdatedAt != now {
		t.Fatalf("unexpected time info: %+v", info)
	}
}

func TestParseSubscriptionUserinfoHeaderMissingIsNil(t *testing.T) {
	info, err := ParseSubscriptionUserinfoHeader(http.Header{}, time.Now())
	if err != nil {
		t.Fatalf("missing header should not error: %v", err)
	}
	if info != nil {
		t.Fatalf("missing header should return nil, got %+v", info)
	}
}

func TestParseSubscriptionUserinfoRejectsInvalidNumbers(t *testing.T) {
	if _, err := ParseSubscriptionUserinfo(`upload=-1; download=0; total=1`, time.Now()); err == nil {
		t.Fatal("expected negative value to be rejected")
	}
	if _, err := ParseSubscriptionUserinfo(`foo=bar`, time.Now()); err == nil {
		t.Fatal("expected header without traffic keys to be rejected")
	}
}

func TestParseSubscriptionInfoFromOutboundsReadsNoticeNodes(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	info := ParseSubscriptionInfoFromOutbounds([]map[string]any{
		{"tag": "剩余流量：66.88 GB", "type": "anytls"},
		{"tag": "距离下次重置剩余：26 天", "type": "anytls"},
		{"tag": "套餐到期：2027-01-01", "type": "anytls"},
	}, now)
	if info == nil {
		t.Fatal("expected subscription info")
	}
	wantRemaining := int64(math.Round(66.88 * 1024 * 1024 * 1024))
	if info.RemainingBytes != wantRemaining || info.TotalBytes != wantRemaining {
		t.Fatalf("unexpected traffic info: %+v", info)
	}
	if info.ExpireUnix != 1798761600 || info.ExpireAt == nil {
		t.Fatalf("unexpected expire info: %+v", info)
	}
	if info.UpdatedAt != now {
		t.Fatalf("unexpected updated_at: %+v", info.UpdatedAt)
	}
}

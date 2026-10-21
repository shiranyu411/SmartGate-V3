package state

import "time"

type SubscriptionInfo struct {
	UploadBytes    int64      `json:"upload_bytes"`
	DownloadBytes  int64      `json:"download_bytes"`
	UsedBytes      int64      `json:"used_bytes"`
	TotalBytes     int64      `json:"total_bytes"`
	RemainingBytes int64      `json:"remaining_bytes"`
	ExpireUnix     int64      `json:"expire_unix,omitempty"`
	ExpireAt       *time.Time `json:"expire_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Raw            string     `json:"raw,omitempty"`
}

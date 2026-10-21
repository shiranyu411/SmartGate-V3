package runtime

var legacyRuntimeFixture = struct {
	ApplyField           string
	CandidateField       string
	LogSource            string
	ServiceRestartOutput string
}{
	ApplyField:           "singbox",
	CandidateField:       "singbox_candidate",
	LogSource:            "sing-box",
	ServiceRestartOutput: "service sing-box restart",
}

package secsdk

// goldenPairs 是 2026-09-14 从一个真实 Chrome 会话抓下来的
// /aweme/v1/web/aweme/detail/ 请求的**全部参数（已解码）**，顺序即发送顺序。
//
// 它同时是两条规则的证据：
//   - 官方页面的参数顺序是 业务参数 → webid → uifid → verifyFp → fp → a_bogus → timestamp
//   - verifyFp 与 fp 是同一个 s_v_web_id 值，重复发两次
//
// 注意最后一项是 timestamp：TestSignGoldenVector 会把它去掉再交给 Sign，
// 因为 timestamp 由 Sign 追加——这正是调用方的用法。
var goldenPairs = [][2]string{
	{"device_platform", "webapp"},
	{"aid", "6383"},
	{"channel", "channel_pc_web"},
	{"aweme_id", "7658147082263416115"},
	{"request_source", "600"},
	{"origin_type", "video_page"},
	{"update_version_code", "170400"},
	{"pc_client_type", "1"},
	{"pc_libra_divert", "Linux"},
	{"support_h265", "0"},
	{"support_dash", "0"},
	{"cpu_core_num", "8"},
	{"version_code", "190500"},
	{"version_name", "19.5.0"},
	{"cookie_enabled", "true"},
	{"screen_width", "1920"},
	{"screen_height", "1080"},
	{"browser_language", "zh-CN"},
	{"browser_platform", "Linux x86_64"},
	{"browser_name", "Chrome"},
	{"browser_version", "131.0.0.0"},
	{"browser_online", "false"},
	{"engine_name", "Blink"},
	{"engine_version", "131.0.0.0"},
	{"os_name", "Windows"},
	{"os_version", "10"},
	{"device_memory", "8"},
	{"platform", "PC"},
	{"downlink", "9.3"},
	{"effective_type", "4g"},
	{"round_trip_time", "0"},
	{"webid", "7685384440256529935"},
	{"uifid", "c4a29131752d59acb78af076c3dbdd52744118e38e80b4b96439ef1e20799db01a0c4ac4a3ea1e0f87c07483346e81f906ccb8fe50c6c49784990a89a174240041b35e8f47651d22c5664965fa04bbe4"},
	{"verifyFp", "verify_mu1aej39_XW6PtZSM_ttLp_4iuq_8xYY_UEZ9UAhhqULD"},
	{"fp", "verify_mu1aej39_XW6PtZSM_ttLp_4iuq_8xYY_UEZ9UAhhqULD"},
	{"a_bogus", "Oy0RkwUJDNQ5Od/SuKrBe92lUS9ArF8yMUixb7aTCOK0LwUY7WNeQNbknoFB4xCWVupshC37xx0lYxxcNUUTpCrkompDu07W6Ynn9hso8qwRG0JQEHRsCwTi9JGclmJwY5KRJ1XW1U8P2V/1w3rkUBl79/BNsOtpsqNjdrUai9F0gMs9T3FQYewQqkLxmaKfJ0787QTwk7AFwE=="},
	{"timestamp", "1789393023"},
}

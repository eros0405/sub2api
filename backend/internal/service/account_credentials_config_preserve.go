package service

// AdminConfigCredentialKeys 列出 Account.Credentials 中属于「管理员配置」而非「授权凭据」的子键。
//
// 这些键由管理员在账号编辑表单里维护，与 OAuth 授权流程无关：重新授权时前端只会带回
// token 类字段（见 useOpenAIOAuth.buildCredentials），因此 incoming 必然不含它们。
// MergePreservingSensitiveCreds 只回填 SensitiveCredentialKeys，配置类键会落在
// 「既不在 incoming、也不在保留清单」的空隙里被静默丢弃——最典型的症状就是
// 重新授权后账号的模型限制（model_mapping）被清空。
//
// Extra 字段早已通过 UpdateAccountExtra 做 key 级合并来防止同类丢失；本清单为
// Credentials 提供对等保护。新增管理员可配置的凭据子键时务必同步此清单。
var AdminConfigCredentialKeys = []string{
	// 模型映射 / 模型限制
	"model_mapping", "compact_model_mapping",
	// 请求头覆写
	"header_overrides", "header_override_enabled",
	// OpenCode Go 协议路由规则
	"protocol_rules",
	// 上游地址覆写
	"base_url", "api_base_urls", "vertex_model_locations",
	// 池化重试策略
	"pool_mode", "pool_mode_retry_count", "pool_mode_retry_status_codes",
	// 自定义错误码 / 临时不可调度规则
	"custom_error_codes", "custom_error_codes_enabled",
	"temp_unschedulable_rules", "temp_unschedulable_enabled",
	// 其他账号级开关
	"intercept_warmup_requests",
}

// PreserveAdminConfigCreds 把 existing 中的管理员配置类子键回填进 incoming（incoming 未显式提供时）。
// 返回新的 map，不修改入参；incoming 为 nil 时返回 nil（交由调用方按「未提供」语义处理）。
//
// 语义与 MergePreservingSensitiveCreds 中敏感键的处理一致：
//   - incoming 显式提供该键：以 incoming 为准（管理员主动修改，包括显式清空）。
//   - incoming 未提供：保留 existing 的值。
func PreserveAdminConfigCreds(existing, incoming map[string]any) map[string]any {
	if incoming == nil {
		return nil
	}
	out := make(map[string]any, len(incoming)+len(AdminConfigCredentialKeys))
	for k, v := range incoming {
		out[k] = v
	}
	for _, key := range AdminConfigCredentialKeys {
		if _, hasIncoming := incoming[key]; hasIncoming {
			continue
		}
		if existingVal, ok := existing[key]; ok {
			out[key] = existingVal
		}
	}
	return out
}

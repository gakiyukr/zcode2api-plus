package gateway

import "strings"

// NormalizeEffort 將 OpenAI / Responses 的思考強度映射為 ZCode 上游接受的檔位。
//
// 上游 GLM-5.4 / GLM-5.3 只認 low / high / max；OpenAI 客戶端常送 medium，
// 直接透傳會以 400 拒絕。此函式只對 GLM 系列生效，未知模型或未知值保持原樣，
// 避免把其他上游的語義一併改寫。
func NormalizeEffort(modelName, effort string) string {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" || !isGLMModel(modelName) {
		return effort
	}
	switch e {
	case "medium":
		// GLM 沒有 medium 檔位；medium 的語義最接近 high。
		return "high"
	case "minimal", "none", "off", "disabled":
		// Flash 強制思考：客戶端關閉思考會讓上游 400，就近歸一為最低檔。
		// 非 Flash 模型不猜測，保留原值交由上游判定。
		if IsFlashModel(modelName) {
			return "low"
		}
		return effort
	case "low", "high", "max":
		return e
	default:
		return effort
	}
}

// IsFlashModel 判斷是否為強制思考的 Flash 模型。
// 上游沒有提供能力位，只能依模型名（含 provider 前綴與大小寫差異）。
func IsFlashModel(modelName string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(modelName)), "flash")
}

// SupportsEffort 回報模型是否使用上游 GLM 的思考檔位；非 GLM 模型不透傳
// output_config，避免把 GLM 專屬參數送到其他上游。
func SupportsEffort(modelName string) bool {
	return isGLMModel(modelName)
}

func isGLMModel(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "glm")
}

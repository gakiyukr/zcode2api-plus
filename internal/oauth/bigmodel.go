// BigModel（智谱国内站）OAuth 常量与 Provider 白名单。
// 端点与参数形态取自 ZCode 桌面端（resources/app.asar 反混淆）：
// - 授权页为 bigmodel.cn 登录页，官方前端把 redirect/appId/state 带进登录流程；
// - 兑换端点与 zai 共用 zcode.z.ai/api/v1/oauth/token，请求体以 provider 区分；
// - 授权完成后同样落回 zcode.z.ai/app/oauth/login?redirect=zcode://oauth/callback，
//   因此回调解析与 zai 完全一致（见 ParseCallbackURL）。
package oauth

import "zcode2api/internal/model"

// bigModelAuthorizeURL bigmodel 登录页（授权入口，非标准 OAuth authorize 端点）。
const bigModelAuthorizeURL = "https://bigmodel.cn/login"

// bigModelAppID 官方前端注册的应用标识（桌面端硬编码 appId=zcode）。
const bigModelAppID = "zcode"

// IsSupportedProvider 登录入口的 provider 白名单。
func IsSupportedProvider(provider string) bool {
	return provider == model.ProviderZai || provider == model.ProviderBigModel
}

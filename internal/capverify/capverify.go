// Package capverify 校验 Cap（自建 PoW 人机验证）的 token。
//
// Cap 是一个自托管的验证码服务：浏览器端跑工作量证明，服务端用
// /siteverify 验证 token。本项目不内置 Cap，只作为客户端调用外部实例——
// 实例地址与密钥由管理员在后台设定，未配置时整个校验被跳过。
//
// 校验走 Cap 的 siteverify 接口，形态与 reCAPTCHA 兼容：
//
//	POST <endpoint>/siteverify
//	{"secret": "<secret key>", "response": "<token>"}
//	→ {"success": true}
//
// token 是一次性的：同一个 token 重复校验必然失败，所以调用方不能缓存结果
// 或重试同一个 token。
package capverify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// verifyTimeout 单次校验的超时上限。
// 校验发生在访客提交路径上，网络慢时宁可放行失败也不能长时间挂住请求。
const verifyTimeout = 10 * time.Second

// Config 一次校验所需的配置。
// Endpoint 是 Cap 实例的公开地址（含 site key），例如
// https://cap.example.com/d9256640cb53/
type Config struct {
	Endpoint string
	Secret   string
}

// Enabled 判断配置是否完整。
// 两项都填齐才启用：只填地址没填密钥会静默放行，等于没验证。
func (c Config) Enabled() bool {
	return strings.TrimSpace(c.Endpoint) != "" && strings.TrimSpace(c.Secret) != ""
}

// ErrNotConfigured 表示未配置 Cap，调用方应跳过校验。
var ErrNotConfigured = errors.New("cap 未配置")

// ErrInvalidToken 表示 Cap 明确拒绝了该 token。
var ErrInvalidToken = errors.New("人机验证未通过")

// Client 校验 Cap token。
type Client struct {
	HTTP *http.Client
}

// New 创建校验客户端。
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: verifyTimeout}}
}

// Verify 校验一个 Cap token。
//
// 返回 ErrNotConfigured 表示未配置（调用方跳过）；返回 ErrInvalidToken 表示
// 校验明确失败；其余错误是网络或解析问题。调用方应把「未配置」与「校验失败」
// 区别对待：前者跳过，后者拒绝。
func (c *Client) Verify(ctx context.Context, cfg Config, token string) error {
	if !cfg.Enabled() {
		return ErrNotConfigured
	}
	if strings.TrimSpace(token) == "" {
		return ErrInvalidToken
	}

	url := strings.TrimSuffix(cfg.Endpoint, "/") + "/siteverify"
	body, err := json.Marshal(map[string]string{
		"secret":   cfg.Secret,
		"response": token,
	})
	if err != nil {
		return fmt.Errorf("构造校验请求失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("构造校验请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("连接人机验证服务失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("人机验证服务返回 %d", resp.StatusCode)
	}

	var out struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("解析校验结果失败: %w", err)
	}
	if !out.Success {
		return ErrInvalidToken
	}
	return nil
}

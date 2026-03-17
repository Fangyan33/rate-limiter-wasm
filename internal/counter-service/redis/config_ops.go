package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"rate-limiter-wasm/internal/counter-service/models"
)

type RateLimitConfig struct {
	Domain        string `json:"domain"`
	APIKey        string `json:"api_key"`
	MaxConcurrent int    `json:"max_concurrent"`
	Enabled       bool   `json:"enabled"`
	Tier          string `json:"tier,omitempty"`
	Description   string `json:"description,omitempty"`
	UpdatedAt     int64  `json:"updated_at,omitempty"`
}

// SetConfig 创建或更新限流配置
func (c *Client) SetConfig(ctx context.Context, cfg models.RateLimitConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	key := c.Key(fmt.Sprintf("config:%s:%s", cfg.Domain, cfg.APIKey))

	fields := map[string]interface{}{
		"max_concurrent": cfg.MaxConcurrent,
		"enabled":        strconv.FormatBool(cfg.Enabled),
		"updated_at":     time.Now().Unix(),
	}

	if cfg.Tier != "" {
		fields["tier"] = cfg.Tier
	}
	if cfg.Description != "" {
		fields["description"] = cfg.Description
	}

	return c.rdb.HSet(ctx, key, fields).Err()
}

// GetConfig 获取单个配置
func (c *Client) GetConfig(ctx context.Context, domain, apiKey string) (*models.RateLimitConfig, error) {
	key := c.Key(fmt.Sprintf("config:%s:%s", domain, apiKey))

	result, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, nil // 不存在返回 nil
	}

	maxConcurrent, _ := strconv.Atoi(result["max_concurrent"])
	enabled := result["enabled"] == "true"
	updatedAt, _ := strconv.ParseInt(result["updated_at"], 10, 64)

	return &models.RateLimitConfig{
		Domain:        domain,
		APIKey:        apiKey,
		MaxConcurrent: maxConcurrent,
		Enabled:       enabled,
		Tier:          result["tier"],
		Description:   result["description"],
		UpdatedAt:     updatedAt,
	}, nil
}

// DeleteConfig 删除指定配置
func (c *Client) DeleteConfig(ctx context.Context, domain, apiKey string) error {
	key := c.Key(fmt.Sprintf("config:%s:%s", domain, apiKey))
	return c.rdb.Del(ctx, key).Err()
}

// ListConfigs 批量查询配置（使用 Lua 脚本分页）
type ConfigListResult struct {
	Cursor  string              `json:"cursor"`
	Configs []models.RateLimitConfig `json:"configs"`
}

func (c *Client) ListConfigs(ctx context.Context, pattern string, cursor uint64, count int64) (*ConfigListResult, error) {
	if count <= 0 {
		count = 100
	}

	result, err := c.rdb.Eval(ctx, GetListConfigsScript(), []string{pattern}, cursor, count).Result()
	if err != nil {
		return nil, err
	}

	jsonStr, ok := result.(string)
	if !ok {
		return nil, fmt.Errorf("list_configs script returned non-string")
	}

	var raw struct {
		Cursor  string            `json:"cursor"`
		Configs []map[string]string `json:"configs"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, err
	}

	configs := make([]models.RateLimitConfig, 0, len(raw.Configs))
	for _, item := range raw.Configs {
		key := item["key"]
		// 解析 key 为 domain:api_key
		parts := strings.SplitN(strings.TrimPrefix(key, c.prefix+"config:"), ":", 2)
		if len(parts) != 2 {
			continue // 跳过非法 key
		}

		maxConcurrent, _ := strconv.Atoi(item["max_concurrent"])
		enabled := item["enabled"] == "true"
		updatedAt, _ := strconv.ParseInt(item["updated_at"], 10, 64)

		configs = append(configs, models.RateLimitConfig{
			Domain:        parts[0],
			APIKey:        parts[1],
			MaxConcurrent: maxConcurrent,
			Enabled:       enabled,
			Tier:          item["tier"],
			Description:   item["description"],
			UpdatedAt:     updatedAt,
		})
	}

	return &ConfigListResult{
		Cursor:  raw.Cursor,
		Configs: configs,
	}, nil
}

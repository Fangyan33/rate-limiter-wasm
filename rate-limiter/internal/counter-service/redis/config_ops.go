package redis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"rate-limiter-wasm/rate-limiter/internal/counter-service/models"
)

// SetConfig 创建或更新限流配置
func (c *Client) SetConfig(ctx context.Context, cfg models.RateLimitConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	key := c.configKey(cfg.Service, cfg.Port, hashAPIKey(cfg.APIKey))

	fields := map[string]interface{}{
		"max_concurrent": cfg.MaxConcurrent,
		"enabled":        strconv.FormatBool(cfg.Enabled),
		"updated_at":     time.Now().Unix(),
	}

	if cfg.Description != "" {
		fields["description"] = cfg.Description
	}

	_, err := c.rdb.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
		pipe.HSet(ctx, key, fields)
		if cfg.DescriptionSet && cfg.Description == "" {
			pipe.HDel(ctx, key, "description")
		}
		return nil
	})
	return err
}

// GetConfig 获取单个配置
func (c *Client) GetConfig(ctx context.Context, service string, port int, apiKey string) (*models.ConfigView, error) {
	if err := models.ValidateService(service, port); err != nil {
		return nil, err
	}
	key := c.configKey(service, port, hashAPIKey(apiKey))

	result, err := c.rdb.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, nil // 不存在返回 nil
	}

	return configView(service, port, hashAPIKey(apiKey), result)
}

func configView(service string, port int, hash string, fields map[string]string) (*models.ConfigView, error) {
	if fields["enabled"] != "true" && fields["enabled"] != "false" {
		return nil, fmt.Errorf("%w: enabled must be true or false", ErrInvalidConfig)
	}
	limit, err := strconv.Atoi(fields["max_concurrent"])
	if err != nil || limit <= 0 || strconv.Itoa(limit) != fields["max_concurrent"] {
		return nil, fmt.Errorf("%w: max_concurrent must be a positive integer", ErrInvalidConfig)
	}
	updatedAt, _ := strconv.ParseInt(fields["updated_at"], 10, 64)
	return &models.ConfigView{
		Service: service, Port: port, APIKeyHash: hash, MaxConcurrent: limit,
		Enabled: fields["enabled"] == "true", Description: fields["description"], UpdatedAt: updatedAt,
	}, nil
}

// DeleteConfig 删除指定配置
func (c *Client) DeleteConfig(ctx context.Context, service string, port int, apiKey string) error {
	if err := models.ValidateService(service, port); err != nil {
		return err
	}
	key := c.configKey(service, port, hashAPIKey(apiKey))
	return c.rdb.Del(ctx, key).Err()
}

// ListConfigs 批量查询配置（使用 Lua 脚本分页）
type ConfigListResult struct {
	Cursor  string              `json:"cursor"`
	Configs []models.ConfigView `json:"configs"`
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
		Cursor  string          `json:"cursor"`
		Configs json.RawMessage `json:"configs"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil, err
	}

	var page []map[string]string
	// Redis cjson 将空 Lua table 编码为 {}，对外仍返回空数组并保留 cursor。
	if !bytes.Equal(bytes.TrimSpace(raw.Configs), []byte("{}")) {
		if err := json.Unmarshal(raw.Configs, &page); err != nil {
			return nil, err
		}
	}
	configs := make([]models.ConfigView, 0, len(page))
	for _, item := range page {
		key := item["key"]
		identity := strings.TrimPrefix(key, c.Key("service:config:"))
		parts := strings.Split(identity, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("%w: malformed config key", ErrInvalidConfig)
		}
		port, err := strconv.Atoi(parts[1])
		if err != nil || strconv.Itoa(port) != parts[1] || models.ValidateService(parts[0], port) != nil || len(parts[2]) != 64 {
			return nil, fmt.Errorf("%w: malformed config key", ErrInvalidConfig)
		}
		view, err := configView(parts[0], port, parts[2], item)
		if err != nil {
			return nil, err
		}
		configs = append(configs, *view)
	}

	return &ConfigListResult{
		Cursor:  raw.Cursor,
		Configs: configs,
	}, nil
}

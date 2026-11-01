package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"workshop-monitor/internal/agent"
	"workshop-monitor/internal/pkglog"
)

// 缓存参数的默认值取自接口规范 §9.4 的冻结值，环境变量可覆盖（附录 B）。
const (
	defaultCacheMaxDays   = 7
	defaultCacheMaxItems  = 300000
	defaultCacheMaxBytes  = 536870912 // 512 MB
	defaultCacheWarnRatio = 0.8
	defaultBackfillBatch  = 500
	defaultBackfillGapMs  = 50
)

// cacheSettings 是本地队列与补传的参数。
type cacheSettings struct {
	spec     agent.CacheSpec
	backfill agent.BackfillSpec
	path     string
}

// loadCacheSettings 读缓存与补传参数。
//
// 三上限与限速都是「数值类配置」，与 007 的其余参数一样走环境变量，
// 不在代码里写死第二个真值。
func loadCacheSettings(dbPath string) (cacheSettings, error) {
	spec, err := loadCacheSpec()
	if err != nil {
		return cacheSettings{}, err
	}
	bf, err := loadBackfillSpec()
	if err != nil {
		return cacheSettings{}, err
	}
	return cacheSettings{path: dbPath, spec: spec, backfill: bf}, nil
}

// loadCacheSpec 读三上限与预警比例（接口规范 §9.4，附录 B）。
func loadCacheSpec() (agent.CacheSpec, error) {
	days, err := intEnv("CACHE_MAX_DAYS", defaultCacheMaxDays)
	if err != nil {
		return agent.CacheSpec{}, err
	}
	items, err := intEnv("CACHE_MAX_ITEMS", defaultCacheMaxItems)
	if err != nil {
		return agent.CacheSpec{}, err
	}
	maxBytes, err := int64Env("CACHE_MAX_BYTES", defaultCacheMaxBytes)
	if err != nil {
		return agent.CacheSpec{}, err
	}
	ratio, err := floatEnv("CACHE_WARN_RATIO", defaultCacheWarnRatio)
	if err != nil {
		return agent.CacheSpec{}, err
	}
	if days <= 0 || items <= 0 || maxBytes <= 0 {
		return agent.CacheSpec{}, fmt.Errorf("缓存上限必须为正数（days=%d items=%d bytes=%d）",
			days, items, maxBytes)
	}
	if ratio <= 0 || ratio > 1 {
		return agent.CacheSpec{}, fmt.Errorf("CACHE_WARN_RATIO=%v 必须在 (0, 1] 之间", ratio)
	}
	return agent.CacheSpec{
		MaxAge:    time.Duration(days) * 24 * time.Hour,
		MaxItems:  items,
		MaxBytes:  maxBytes,
		WarnRatio: ratio,
		DropBatch: agent.DefaultCacheDropBatch,
	}, nil
}

// loadBackfillSpec 读补传限速（接口规范 §9.3 第 5 条）。
func loadBackfillSpec() (agent.BackfillSpec, error) {
	batch, err := intEnv("BACKFILL_BATCH", defaultBackfillBatch)
	if err != nil {
		return agent.BackfillSpec{}, err
	}
	gapMs, err := intEnv("BACKFILL_INTERVAL_MS", defaultBackfillGapMs)
	if err != nil {
		return agent.BackfillSpec{}, err
	}
	if batch <= 0 || gapMs <= 0 {
		return agent.BackfillSpec{}, fmt.Errorf("补传限速必须为正数（batch=%d gapMs=%d）", batch, gapMs)
	}
	return agent.BackfillSpec{Batch: batch, Gap: time.Duration(gapMs) * time.Millisecond}, nil
}

// int64Env 读一个 int64 环境变量，缺省用 fallback。
func int64Env(key string, fallback int64) (int64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("环境变量 %s=%q 不是整数: %w", key, raw, err)
	}
	return v, nil
}

// floatEnv 读一个浮点环境变量，缺省用 fallback。
func floatEnv(key string, fallback float64) (float64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("环境变量 %s=%q 不是数字: %w", key, raw, err)
	}
	return v, nil
}

// openCache 打开本地队列并建好目录。默认路径 var/cache.db 在首次部署时还没有 var/。
//
// path 为空表示不落盘：断网期间数据只在内存里，退出即丢。生产装配必须给路径。
func openCache(path string, spec agent.CacheSpec, log *pkglog.Logger) (*agent.Cache, error) {
	if path == "" {
		log.Warn("未配置本地队列路径，断网期间的数据不会落盘")
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("建缓存目录 %s: %w", filepath.Dir(path), err)
	}
	return agent.OpenCache(path, spec)
}

// resumeSeq 把序号高水位抬到 max(文件, 队列最大 seq)。
func resumeSeq(alloc *agent.SeqAllocator, cache *agent.Cache) error {
	maxSeq, err := cache.MaxSeq()
	if err != nil {
		return fmt.Errorf("读队列最大序号: %w", err)
	}
	if err := alloc.ResumeTo(maxSeq); err != nil {
		return fmt.Errorf("按队列续接序号: %w", err)
	}
	return nil
}

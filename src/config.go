package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/spf13/viper"
	"github.com/fsnotify/fsnotify"
)

// RegistryMapping Registry映射配置
type RegistryMapping struct {
	Upstream string `toml:"upstream"` // 上游Registry地址
	AuthHost string `toml:"authHost"` // 认证服务器地址
	AuthType string `toml:"authType"` // 认证类型: docker/github/google/basic
	Enabled  bool   `toml:"enabled"`  // 是否启用
}

// AppConfig 应用配置结构体
type AppConfig struct {
	Server struct {
		Host     string `toml:"host"`     // 监听地址
		Port     int    `toml:"port"`     // 监听端口
		FileSize int64  `toml:"fileSize"` // 文件大小限制（字节）
	} `toml:"server"`

	RateLimit struct {
		RequestLimit int     `toml:"requestLimit"` // 每小时请求限制
		PeriodHours  float64 `toml:"periodHours"`  // 限制周期（小时）
	} `toml:"rateLimit"`

	Security struct {
		WhiteList []string `toml:"whiteList"` // 白名单IP/CIDR列表
		BlackList []string `toml:"blackList"` // 黑名单IP/CIDR列表
	} `toml:"security"`

	Proxy struct {
		WhiteList []string `toml:"whiteList"` // 代理白名单（仓库级别）
		BlackList []string `toml:"blackList"` // 代理黑名单（仓库级别）
	} `toml:"proxy"`

	Download struct {
		MaxImages int `toml:"maxImages"` // 单次下载最大镜像数量限制
	} `toml:"download"`

	Registries map[string]RegistryMapping `toml:"registries"`

	TokenCache struct {
		Enabled    bool   `toml:"enabled"`    // 是否启用token缓存
		DefaultTTL string `toml:"defaultTTL"` // 默认缓存时间
	} `toml:"tokenCache"`
}

var (
	appConfig     *AppConfig
	appConfigLock sync.RWMutex
	isViperEnabled bool
	viperInstance  *viper.Viper
	
	cachedConfig     *AppConfig
	configCacheTime  time.Time
	configCacheTTL   = 5 * time.Second
	configCacheMutex sync.RWMutex
)

// DefaultConfig 返回默认配置
func DefaultConfig() *AppConfig {
	return &AppConfig{
		Server: struct {
			Host     string `toml:"host"`
			Port     int    `toml:"port"`
			FileSize int64  `toml:"fileSize"`
		}{
			Host:     "0.0.0.0",
			Port:     5000,
			FileSize: 2 * 1024 * 1024 * 1024, // 2GB
		},
		RateLimit: struct {
			RequestLimit int     `toml:"requestLimit"`
			PeriodHours  float64 `toml:"periodHours"`
		}{
			RequestLimit: 20,
			PeriodHours:  1.0,
		},
		Security: struct {
			WhiteList []string `toml:"whiteList"`
			BlackList []string `toml:"blackList"`
		}{
			WhiteList: []string{},
			BlackList: []string{},
		},
		Proxy: struct {
			WhiteList []string `toml:"whiteList"`
			BlackList []string `toml:"blackList"`
		}{
			WhiteList: []string{},
			BlackList: []string{},
		},
		Download: struct {
			MaxImages int `toml:"maxImages"`
		}{
			MaxImages: 10, // 默认值：最多同时下载10个镜像
		},
		Registries: map[string]RegistryMapping{
			"ghcr.io": {
				Upstream: "ghcr.io",
				AuthHost: "ghcr.io/token",
				AuthType: "github",
				Enabled:  true,
			},
			"gcr.io": {
				Upstream: "gcr.io",
				AuthHost: "gcr.io/v2/token",
				AuthType: "google",
				Enabled:  true,
			},
			"quay.io": {
				Upstream: "quay.io",
				AuthHost: "quay.io/v2/auth",
				AuthType: "quay",
				Enabled:  true,
			},
			"registry.k8s.io": {
				Upstream: "registry.k8s.io",
				AuthHost: "registry.k8s.io",
				AuthType: "anonymous",
				Enabled:  true,
			},
		},
		TokenCache: struct {
			Enabled    bool   `toml:"enabled"`
			DefaultTTL string `toml:"defaultTTL"`
		}{
			Enabled:    true, // docker认证的匿名Token缓存配置，用于提升性能
			DefaultTTL: "20m",
		},
	}
}

// GetConfig 安全地获取配置副本
func GetConfig() *AppConfig {
	configCacheMutex.RLock()
	if cachedConfig != nil && time.Since(configCacheTime) < configCacheTTL {
		config := cachedConfig
		configCacheMutex.RUnlock()
		return config
	}
	configCacheMutex.RUnlock()
	
	// 缓存过期，重新生成配置
	configCacheMutex.Lock()
	defer configCacheMutex.Unlock()
	
	// 双重检查，防止重复生成
	if cachedConfig != nil && time.Since(configCacheTime) < configCacheTTL {
		return cachedConfig
	}
	
	appConfigLock.RLock()
	if appConfig == nil {
		appConfigLock.RUnlock()
		defaultCfg := DefaultConfig()
		cachedConfig = defaultCfg
		configCacheTime = time.Now()
		return defaultCfg
	}
	
	// 生成新的配置深拷贝
	configCopy := *appConfig
	configCopy.Security.WhiteList = append([]string(nil), appConfig.Security.WhiteList...)
	configCopy.Security.BlackList = append([]string(nil), appConfig.Security.BlackList...)
	configCopy.Proxy.WhiteList = append([]string(nil), appConfig.Proxy.WhiteList...)
	configCopy.Proxy.BlackList = append([]string(nil), appConfig.Proxy.BlackList...)
	appConfigLock.RUnlock()
	
	cachedConfig = &configCopy
	configCacheTime = time.Now()
	
	return cachedConfig
}

// setConfig 安全地设置配置
func setConfig(cfg *AppConfig) {
	appConfigLock.Lock()
	defer appConfigLock.Unlock()
	appConfig = cfg
	
	configCacheMutex.Lock()
	cachedConfig = nil
	configCacheMutex.Unlock()
}

// LoadConfig 加载配置文件
func LoadConfig() error {
	// 首先使用默认配置
	cfg := DefaultConfig()
	
	// 尝试加载TOML配置文件
	if data, err := os.ReadFile("config.toml"); err == nil {
		if err := toml.Unmarshal(data, cfg); err != nil {
			return fmt.Errorf("解析配置文件失败: %v", err)
		}
	} else {
		fmt.Println("未找到config.toml，使用默认配置")
	}
	
	// 从环境变量覆盖配置
	overrideFromEnv(cfg)
	
	// 设置配置
	setConfig(cfg)
	
	if !isViperEnabled {
		go enableViperHotReload()
	}
	
	return nil
}

func enableViperHotReload() {
	if isViperEnabled {
		return
	}
	
	// 创建Viper实例
	viperInstance = viper.New()
	
	// 配置Viper
	viperInstance.SetConfigName("config")
	viperInstance.SetConfigType("toml")
	viperInstance.AddConfigPath(".")
	
	// 读取配置文件
	if err := viperInstance.ReadInConfig(); err != nil {
		fmt.Printf("读取配置失败，继续使用当前配置: %v\n", err)
		return
	}
	
	isViperEnabled = true
	
	viperInstance.WatchConfig()
	viperInstance.OnConfigChange(func(e fsnotify.Event) {
		fmt.Printf("检测到配置文件变化: %s\n", e.Name)
		hotReloadWithViper()
	})
}

func hotReloadWithViper() {
	start := time.Now()
	fmt.Println("🔄 自动热重载...")
	
	// 创建新配置
	cfg := DefaultConfig()
	
	// 使用Viper解析配置到结构体
	if err := viperInstance.Unmarshal(cfg); err != nil {
		fmt.Printf("❌ 配置解析失败: %v\n", err)
		return
	}
	
	overrideFromEnv(cfg)
	
	setConfig(cfg)
	
	// 异步更新受影响的组件
	go func() {
		updateAffectedComponents()
		fmt.Printf("✅ Viper配置热重载完成，耗时: %v\n", time.Since(start))
	}()
}

func updateAffectedComponents() {
	// 重新初始化限流器
	if globalLimiter != nil {
		fmt.Println("📡 重新初始化限流器...")
		initLimiter()
	}
	
	// 重新加载访问控制
	fmt.Println("🔒 重新加载访问控制规则...")
	if GlobalAccessController != nil {
		GlobalAccessController.Reload()
	}
	
	fmt.Println("🌐 更新Registry配置映射...")
	reloadRegistryConfig()
	
	// 其他需要重新初始化的组件可以在这里添加
	fmt.Println("🔧 组件更新完成")
}

func reloadRegistryConfig() {
	cfg := GetConfig()
	enabledCount := 0
	
	// 统计启用的Registry数量
	for _, mapping := range cfg.Registries {
		if mapping.Enabled {
			enabledCount++
		}
	}
	
	fmt.Printf("🌐 Registry配置已更新: %d个启用\n", enabledCount)
	
}

// overrideFromEnv 从环境变量覆盖配置
func overrideFromEnv(cfg *AppConfig) {
	// 服务器配置
	if val := os.Getenv("SERVER_HOST"); val != "" {
		cfg.Server.Host = val
	}
	if val := os.Getenv("SERVER_PORT"); val != "" {
		if port, err := strconv.Atoi(val); err == nil && port > 0 {
			cfg.Server.Port = port
		}
	}
	if val := os.Getenv("MAX_FILE_SIZE"); val != "" {
		if size, err := strconv.ParseInt(val, 10, 64); err == nil && size > 0 {
			cfg.Server.FileSize = size
		}
	}
	
	// 限流配置
	if val := os.Getenv("RATE_LIMIT"); val != "" {
		if limit, err := strconv.Atoi(val); err == nil && limit > 0 {
			cfg.RateLimit.RequestLimit = limit
		}
	}
	if val := os.Getenv("RATE_PERIOD_HOURS"); val != "" {
		if period, err := strconv.ParseFloat(val, 64); err == nil && period > 0 {
			cfg.RateLimit.PeriodHours = period
		}
	}
	
	// IP限制配置
	if val := os.Getenv("IP_WHITELIST"); val != "" {
		cfg.Security.WhiteList = append(cfg.Security.WhiteList, strings.Split(val, ",")...)
	}
	if val := os.Getenv("IP_BLACKLIST"); val != "" {
		cfg.Security.BlackList = append(cfg.Security.BlackList, strings.Split(val, ",")...)
	}
	
	// 下载限制配置
	if val := os.Getenv("MAX_IMAGES"); val != "" {
		if maxImages, err := strconv.Atoi(val); err == nil && maxImages > 0 {
			cfg.Download.MaxImages = maxImages
		}
	}
}

// CreateDefaultConfigFile 创建默认配置文件
func CreateDefaultConfigFile() error {
	cfg := DefaultConfig()
	
	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化默认配置失败: %v", err)
	}
	
	return os.WriteFile("config.toml", data, 0644)
} 
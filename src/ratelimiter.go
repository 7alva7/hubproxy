package main

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

const (
	// 清理间隔
	CleanupInterval = 10 * time.Minute
	MaxIPCacheSize = 10000
)

// IPRateLimiter IP限流器结构体
type IPRateLimiter struct {
	ips       map[string]*rateLimiterEntry // IP到限流器的映射
	mu        *sync.RWMutex                // 读写锁，保证并发安全
	r         rate.Limit                   // 速率限制（每秒允许的请求数）
	b         int                          // 令牌桶容量（突发请求数）
	whitelist []*net.IPNet                 // 白名单IP段
	blacklist []*net.IPNet                 // 黑名单IP段
}

// rateLimiterEntry 限流器条目
type rateLimiterEntry struct {
	limiter    *rate.Limiter
	lastAccess time.Time
}

// initGlobalLimiter 初始化全局限流器
func initGlobalLimiter() *IPRateLimiter {
	cfg := GetConfig()
	
	whitelist := make([]*net.IPNet, 0, len(cfg.Security.WhiteList))
	for _, item := range cfg.Security.WhiteList {
		if item = strings.TrimSpace(item); item != "" {
			if !strings.Contains(item, "/") {
				item = item + "/32" // 单个IP转为CIDR格式
			}
			_, ipnet, err := net.ParseCIDR(item)
			if err == nil {
				whitelist = append(whitelist, ipnet)
			} else {
				fmt.Printf("警告: 无效的白名单IP格式: %s\n", item)
			}
		}
	}
	
	// 解析黑名单IP段
	blacklist := make([]*net.IPNet, 0, len(cfg.Security.BlackList))
	for _, item := range cfg.Security.BlackList {
		if item = strings.TrimSpace(item); item != "" {
			if !strings.Contains(item, "/") {
				item = item + "/32" // 单个IP转为CIDR格式
			}
			_, ipnet, err := net.ParseCIDR(item)
			if err == nil {
				blacklist = append(blacklist, ipnet)
			} else {
				fmt.Printf("警告: 无效的黑名单IP格式: %s\n", item)
			}
		}
	}
	
	// 计算速率：将 "每N小时X个请求" 转换为 "每秒Y个请求"
	ratePerSecond := rate.Limit(float64(cfg.RateLimit.RequestLimit) / (cfg.RateLimit.PeriodHours * 3600))
	
	burstSize := cfg.RateLimit.RequestLimit
	if burstSize < 1 {
		burstSize = 1
	}
	
	limiter := &IPRateLimiter{
		ips:       make(map[string]*rateLimiterEntry),
		mu:        &sync.RWMutex{},
		r:         ratePerSecond,
		b:         burstSize,
		whitelist: whitelist,
		blacklist: blacklist,
	}
	
	// 启动定期清理goroutine
	go limiter.cleanupRoutine()
	
	return limiter
}

// initLimiter 初始化限流器
func initLimiter() {
	globalLimiter = initGlobalLimiter()
}

// cleanupRoutine 定期清理过期的限流器
func (i *IPRateLimiter) cleanupRoutine() {
	ticker := time.NewTicker(CleanupInterval)
	defer ticker.Stop()
	
	for range ticker.C {
		now := time.Now()
		expired := make([]string, 0)
		
		// 查找过期的条目
		i.mu.RLock()
		for ip, entry := range i.ips {
			// 如果最后访问时间超过1小时，认为过期
			if now.Sub(entry.lastAccess) > 1*time.Hour {
				expired = append(expired, ip)
			}
		}
		i.mu.RUnlock()
		
		// 如果有过期条目或者缓存过大，进行清理
		if len(expired) > 0 || len(i.ips) > MaxIPCacheSize {
			i.mu.Lock()
			// 删除过期条目
			for _, ip := range expired {
				delete(i.ips, ip)
			}
			
			// 如果缓存仍然过大，全部清理
			if len(i.ips) > MaxIPCacheSize {
				i.ips = make(map[string]*rateLimiterEntry)
			}
			i.mu.Unlock()
		}
	}
}

// extractIPFromAddress 从地址中提取纯IP
func extractIPFromAddress(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

// normalizeIPForRateLimit 标准化IP地址用于限流：IPv4保持不变，IPv6标准化为/64网段
func normalizeIPForRateLimit(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ipStr // 解析失败，返回原值
	}
	
	if ip.To4() != nil {
		return ipStr // IPv4保持不变
	}
	
	// IPv6：标准化为 /64 网段
	ipv6 := ip.To16()
	for i := 8; i < 16; i++ {
		ipv6[i] = 0 // 清零后64位
	}
	return ipv6.String() + "/64"
}

// isIPInCIDRList 检查IP是否在CIDR列表中
func isIPInCIDRList(ip string, cidrList []*net.IPNet) bool {
	// 先提取纯IP地址
	cleanIP := extractIPFromAddress(ip)
	parsedIP := net.ParseIP(cleanIP)
	if parsedIP == nil {
		return false
	}
	
	for _, cidr := range cidrList {
		if cidr.Contains(parsedIP) {
			return true
		}
	}
	return false
}

// GetLimiter 获取指定IP的限流器，同时返回是否允许访问
func (i *IPRateLimiter) GetLimiter(ip string) (*rate.Limiter, bool) {
	// 提取纯IP地址
	cleanIP := extractIPFromAddress(ip)
	
	// 检查是否在黑名单中
	if isIPInCIDRList(cleanIP, i.blacklist) {
		return nil, false
	}
	
	// 检查是否在白名单中
	if isIPInCIDRList(cleanIP, i.whitelist) {
		return rate.NewLimiter(rate.Inf, i.b), true
	}
	
	// 标准化IP用于限流：IPv4保持不变，IPv6标准化为/64网段
	normalizedIP := normalizeIPForRateLimit(cleanIP)
	
	now := time.Now()
	
	i.mu.RLock()
	entry, exists := i.ips[normalizedIP]
	i.mu.RUnlock()
	
	if exists {
		i.mu.Lock()
		if entry, stillExists := i.ips[normalizedIP]; stillExists {
			entry.lastAccess = now
			i.mu.Unlock()
			return entry.limiter, true
		}
		i.mu.Unlock()
	}
	
	i.mu.Lock()
	if entry, exists := i.ips[normalizedIP]; exists {
		entry.lastAccess = now
		i.mu.Unlock()
		return entry.limiter, true
	}
	
	entry = &rateLimiterEntry{
		limiter:    rate.NewLimiter(i.r, i.b),
		lastAccess: now,
	}
	i.ips[normalizedIP] = entry
	i.mu.Unlock()
	
	return entry.limiter, true
}

// RateLimitMiddleware 速率限制中间件
func RateLimitMiddleware(limiter *IPRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 静态文件豁免：跳过限流检查
		path := c.Request.URL.Path
		if path == "/" || path == "/favicon.ico" || path == "/images.html" || path == "/search.html" || 
		   strings.HasPrefix(path, "/public/") {
			c.Next()
			return
		}

		// 获取客户端真实IP
		var ip string
		
		// 优先尝试从请求头获取真实IP
		if forwarded := c.GetHeader("X-Forwarded-For"); forwarded != "" {
			// X-Forwarded-For可能包含多个IP，取第一个
			ips := strings.Split(forwarded, ",")
			ip = strings.TrimSpace(ips[0])
		} else if realIP := c.GetHeader("X-Real-IP"); realIP != "" {
			// 如果有X-Real-IP头
			ip = realIP
		} else if remoteIP := c.GetHeader("X-Original-Forwarded-For"); remoteIP != "" {
			// 某些代理可能使用此头
			ips := strings.Split(remoteIP, ",")
			ip = strings.TrimSpace(ips[0])
		} else {
			// 回退到ClientIP方法
			ip = c.ClientIP()
		}
		
		// 提取纯IP地址（去除可能存在的端口）
		cleanIP := extractIPFromAddress(ip)
		
		// 日志记录请求IP和头信息
		normalizedIP := normalizeIPForRateLimit(cleanIP)
		if cleanIP != normalizedIP {
			fmt.Printf("请求IP: %s (提纯后: %s, 限流段: %s), X-Forwarded-For: %s, X-Real-IP: %s\n", 
				ip, cleanIP, normalizedIP,
				c.GetHeader("X-Forwarded-For"), 
				c.GetHeader("X-Real-IP"))
		} else {
			fmt.Printf("请求IP: %s (提纯后: %s), X-Forwarded-For: %s, X-Real-IP: %s\n", 
				ip, cleanIP,
				c.GetHeader("X-Forwarded-For"), 
				c.GetHeader("X-Real-IP"))
		}
		
		// 获取限流器并检查是否允许访问
		ipLimiter, allowed := limiter.GetLimiter(cleanIP)
		
		// 如果IP在黑名单中
		if !allowed {
			c.JSON(403, gin.H{
				"error": "您已被限制访问",
			})
			c.Abort()
			return
		}
		
		// 检查限流
		if !ipLimiter.Allow() {
			c.JSON(429, gin.H{
				"error": "请求频率过快，暂时限制访问",
			})
			c.Abort()
			return
		}
		
		c.Next()
	}
}



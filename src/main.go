package main

import (
	"embed"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed public/*
var staticFiles embed.FS

// 服务嵌入的静态文件
func serveEmbedFile(c *gin.Context, filename string) {
	data, err := staticFiles.ReadFile(filename)
	if err != nil {
		c.Status(404)
		return
	}
	contentType := "text/html; charset=utf-8"
	if strings.HasSuffix(filename, ".ico") {
		contentType = "image/x-icon"
	}
	c.Data(200, contentType, data)
}

var (
	exps = []*regexp.Regexp{
		regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+)/([^/]+)/(?:releases|archive)/.*$`),
		regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+)/([^/]+)/(?:blob|raw)/.*$`),
		regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+)/([^/]+)/(?:info|git-).*$`),
		regexp.MustCompile(`^(?:https?://)?raw\.github(?:usercontent|)\.com/([^/]+)/([^/]+)/.+?/.+$`),
		regexp.MustCompile(`^(?:https?://)?gist\.github(?:usercontent|)\.com/([^/]+)/.+?/.+`),
		regexp.MustCompile(`^(?:https?://)?api\.github\.com/repos/([^/]+)/([^/]+)/.*`),
		regexp.MustCompile(`^(?:https?://)?huggingface\.co(?:/spaces)?/([^/]+)/(.+)$`),
		regexp.MustCompile(`^(?:https?://)?cdn-lfs\.hf\.co(?:/spaces)?/([^/]+)/([^/]+)(?:/(.*))?$`),
		regexp.MustCompile(`^(?:https?://)?download\.docker\.com/([^/]+)/.*\.(tgz|zip)$`),
		regexp.MustCompile(`^(?:https?://)?(github|opengraph)\.githubassets\.com/([^/]+)/.+?$`),
	}
	globalLimiter *IPRateLimiter

	// 服务启动时间
	serviceStartTime = time.Now()
)

func main() {
	// 加载配置
	if err := LoadConfig(); err != nil {
		fmt.Printf("配置加载失败: %v\n", err)
		return
	}

	// 初始化HTTP客户端
	initHTTPClients()

	// 初始化限流器
	initLimiter()

	// 初始化Docker流式代理
	initDockerProxy()

	// 初始化镜像流式下载器
	initImageStreamer()

	// 初始化防抖器
	initDebouncer()

	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

	// 全局Panic恢复保护
	router.Use(gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		log.Printf("🚨 Panic recovered: %v", recovered)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Internal server error",
			"code":  "INTERNAL_ERROR",
		})
	}))

	// 全局限流中间件 - 应用到所有路由
	router.Use(RateLimitMiddleware(globalLimiter))

	// 初始化监控端点
	initHealthRoutes(router)

	// 初始化镜像tar下载路由
	initImageTarRoutes(router)

	// 静态文件路由
	router.GET("/", func(c *gin.Context) {
		serveEmbedFile(c, "public/index.html")
	})
	router.GET("/public/*filepath", func(c *gin.Context) {
		filepath := strings.TrimPrefix(c.Param("filepath"), "/")
		serveEmbedFile(c, "public/"+filepath)
	})

	router.GET("/images.html", func(c *gin.Context) {
		serveEmbedFile(c, "public/images.html")
	})
	router.GET("/search.html", func(c *gin.Context) {
		serveEmbedFile(c, "public/search.html")
	})
	router.GET("/favicon.ico", func(c *gin.Context) {
		serveEmbedFile(c, "public/favicon.ico")
	})

	// 注册dockerhub搜索路由
	RegisterSearchRoute(router)

	// 注册Docker认证路由（/token*）
	router.Any("/token", ProxyDockerAuthGin)
	router.Any("/token/*path", ProxyDockerAuthGin)

	// 注册Docker Registry代理路由
	router.Any("/v2/*path", ProxyDockerRegistryGin)

	// 注册NoRoute处理器
	router.NoRoute(handler)

	cfg := GetConfig()
	fmt.Printf("🚀 HubProxy 启动成功\n")
	fmt.Printf("📡 监听地址: %s:%d\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Printf("⚡ 限流配置: %d请求/%g小时\n", cfg.RateLimit.RequestLimit, cfg.RateLimit.PeriodHours)
	fmt.Printf("🔗 项目地址: https://github.com/sky22333/hubproxy\n")

	err := router.Run(fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port))
	if err != nil {
		fmt.Printf("启动服务失败: %v\n", err)
	}
}

func handler(c *gin.Context) {
	rawPath := strings.TrimPrefix(c.Request.URL.RequestURI(), "/")

	for strings.HasPrefix(rawPath, "/") {
		rawPath = strings.TrimPrefix(rawPath, "/")
	}
	// 自动补全协议头
	if !strings.HasPrefix(rawPath, "https://") {
		// 修复 http:/ 和 https:/ 的情况
		if strings.HasPrefix(rawPath, "http:/") || strings.HasPrefix(rawPath, "https:/") {
			rawPath = strings.Replace(rawPath, "http:/", "", 1)
			rawPath = strings.Replace(rawPath, "https:/", "", 1)
		} else if strings.HasPrefix(rawPath, "http://") {
			rawPath = strings.TrimPrefix(rawPath, "http://")
		}
		rawPath = "https://" + rawPath
	}
	
	matches := checkURL(rawPath)
	if matches != nil {
		// GitHub仓库访问控制检查
		if allowed, reason := GlobalAccessController.CheckGitHubAccess(matches); !allowed {
			// 构建仓库名用于日志
			var repoPath string
			if len(matches) >= 2 {
				username := matches[0]
				repoName := strings.TrimSuffix(matches[1], ".git")
				repoPath = username + "/" + repoName
			}
			fmt.Printf("GitHub仓库 %s 访问被拒绝: %s\n", repoPath, reason)
			c.String(http.StatusForbidden, reason)
			return
		}
	} else {
		c.String(http.StatusForbidden, "无效输入")
		return
	}

	if exps[1].MatchString(rawPath) {
		rawPath = strings.Replace(rawPath, "/blob/", "/raw/", 1)
	}

	proxyRequest(c, rawPath)
}

func proxyRequest(c *gin.Context, u string) {
	proxyWithRedirect(c, u, 0)
}

func proxyWithRedirect(c *gin.Context, u string, redirectCount int) {
	// 限制最大重定向次数，防止无限递归
	const maxRedirects = 20
	if redirectCount > maxRedirects {
		c.String(http.StatusLoopDetected, "重定向次数过多，可能存在循环重定向")
		return
	}
	req, err := http.NewRequest(c.Request.Method, u, c.Request.Body)
	if err != nil {
		c.String(http.StatusInternalServerError, fmt.Sprintf("server error %v", err))
		return
	}

	for key, values := range c.Request.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Del("Host")

	resp, err := GetGlobalHTTPClient().Do(req)
	if err != nil {
		c.String(http.StatusInternalServerError, fmt.Sprintf("server error %v", err))
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Printf("关闭响应体失败: %v\n", err)
		}
	}()

	// 检查文件大小限制
	cfg := GetConfig()
	if contentLength := resp.Header.Get("Content-Length"); contentLength != "" {
		if size, err := strconv.ParseInt(contentLength, 10, 64); err == nil && size > cfg.Server.FileSize {
			c.String(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("文件过大，限制大小: %d MB", cfg.Server.FileSize/(1024*1024)))
			return
		}
	}

	// 清理安全相关的头
	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("Referrer-Policy")
	resp.Header.Del("Strict-Transport-Security")

	// 获取真实域名
	realHost := c.Request.Header.Get("X-Forwarded-Host")
	if realHost == "" {
		realHost = c.Request.Host
	}
	// 如果域名中没有协议前缀，添加https://
	if !strings.HasPrefix(realHost, "http://") && !strings.HasPrefix(realHost, "https://") {
		realHost = "https://" + realHost
	}

	if strings.HasSuffix(strings.ToLower(u), ".sh") {
		isGzipCompressed := resp.Header.Get("Content-Encoding") == "gzip"

		processedBody, processedSize, err := ProcessSmart(resp.Body, isGzipCompressed, realHost)
		if err != nil {
			fmt.Printf("智能处理失败，回退到直接代理: %v\n", err)
			processedBody = resp.Body
			processedSize = 0
		}

		// 智能设置响应头
		if processedSize > 0 {
			resp.Header.Del("Content-Length")
			resp.Header.Del("Content-Encoding")
			resp.Header.Set("Transfer-Encoding", "chunked")
		}

		// 复制其他响应头
		for key, values := range resp.Header {
			for _, value := range values {
				c.Header(key, value)
			}
		}

		if location := resp.Header.Get("Location"); location != "" {
			if checkURL(location) != nil {
				c.Header("Location", "/"+location)
			} else {
				proxyWithRedirect(c, location, redirectCount+1)
				return
			}
		}

		c.Status(resp.StatusCode)

		// 输出处理后的内容
		if _, err := io.Copy(c.Writer, processedBody); err != nil {
			return
		}
	} else {
		for key, values := range resp.Header {
			for _, value := range values {
				c.Header(key, value)
			}
		}

		// 处理重定向
		if location := resp.Header.Get("Location"); location != "" {
			if checkURL(location) != nil {
				c.Header("Location", "/"+location)
			} else {
				proxyWithRedirect(c, location, redirectCount+1)
				return
			}
		}

		c.Status(resp.StatusCode)

		// 直接流式转发
		io.Copy(c.Writer, resp.Body)
	}
}

func checkURL(u string) []string {
	for _, exp := range exps {
		if matches := exp.FindStringSubmatch(u); matches != nil {
			return matches[1:]
		}
	}
	return nil
}

// 简单的健康检查
func formatBeijingTime(t time.Time) string {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600) // 兜底时区
	}
	return t.In(loc).Format("2006-01-02 15:04:05")
}

// 转换为可读时间
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d秒", int(d.Seconds()))
	} else if d < time.Hour {
		return fmt.Sprintf("%d分钟%d秒", int(d.Minutes()), int(d.Seconds())%60)
	} else if d < 24*time.Hour {
		return fmt.Sprintf("%d小时%d分钟", int(d.Hours()), int(d.Minutes())%60)
	} else {
		days := int(d.Hours()) / 24
		hours := int(d.Hours()) % 24
		return fmt.Sprintf("%d天%d小时", days, hours)
	}
}

func initHealthRoutes(router *gin.Engine) {
	router.GET("/health", func(c *gin.Context) {
		uptime := time.Since(serviceStartTime)
		c.JSON(http.StatusOK, gin.H{
			"status":         "healthy",
			"timestamp_unix": serviceStartTime.Unix(),
			"uptime_sec":     uptime.Seconds(),
			"service":        "hubproxy",
			"start_time_bj":  formatBeijingTime(serviceStartTime),
			"uptime_human":   formatDuration(uptime),
		})
	})

	router.GET("/ready", func(c *gin.Context) {
		uptime := time.Since(serviceStartTime)
		c.JSON(http.StatusOK, gin.H{
			"ready":          true,
			"timestamp_unix": time.Now().Unix(),
			"uptime_sec":     uptime.Seconds(),
			"uptime_human":   formatDuration(uptime),
		})
	})
}

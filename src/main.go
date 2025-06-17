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
	router.Any("/token", RateLimitMiddleware(globalLimiter), ProxyDockerAuthGin)
	router.Any("/token/*path", RateLimitMiddleware(globalLimiter), ProxyDockerAuthGin)
	
	// 注册Docker Registry代理路由
	router.Any("/v2/*path", RateLimitMiddleware(globalLimiter), ProxyDockerRegistryGin)
	

	// 注册NoRoute处理器
	router.NoRoute(RateLimitMiddleware(globalLimiter), handler)

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

	if !strings.HasPrefix(rawPath, "http") {
		c.String(http.StatusForbidden, "无效输入")
		return
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

// 初始化健康监控路由
func initHealthRoutes(router *gin.Engine) {
	// 健康检查端点
	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":    "healthy",
			"timestamp": time.Now().Unix(),
			"uptime":    time.Since(serviceStartTime).Seconds(),
			"service":   "hubproxy",
		})
	})
	
	// 就绪检查端点
	router.GET("/ready", func(c *gin.Context) {
		checks := make(map[string]string)
		allReady := true
		
		if GetConfig() != nil {
			checks["config"] = "ok"
		} else {
			checks["config"] = "failed"
			allReady = false
		}
		
		// 检查全局缓存状态
		if globalCache != nil {
			checks["cache"] = "ok"
		} else {
			checks["cache"] = "failed"
			allReady = false
		}
		
		// 检查限流器状态
		if globalLimiter != nil {
			checks["ratelimiter"] = "ok"
		} else {
			checks["ratelimiter"] = "failed"
			allReady = false
		}
		
		// 检查镜像下载器状态
		if globalImageStreamer != nil {
			checks["imagestreamer"] = "ok"
		} else {
			checks["imagestreamer"] = "failed"
			allReady = false
		}
		
		// 检查HTTP客户端状态
		if GetGlobalHTTPClient() != nil {
			checks["httpclient"] = "ok"
		} else {
			checks["httpclient"] = "failed"
			allReady = false
		}
		
		status := http.StatusOK
		if !allReady {
			status = http.StatusServiceUnavailable
		}
		
		c.JSON(status, gin.H{
			"ready":     allReady,
			"checks":    checks,
			"timestamp": time.Now().Unix(),
			"uptime":    time.Since(serviceStartTime).Seconds(),
		})
	})
}

package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

// requestID 注入并回传 X-Request-Id，用于诊断而不泄漏内部细节。
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader("X-Request-Id")
		if rid == "" {
			var b [16]byte
			_, _ = rand.Read(b[:])
			rid = hex.EncodeToString(b[:])
		}
		c.Set("request_id", rid)
		c.Header("X-Request-Id", rid)
		c.Next()
	}
}

// recovery 统一 panic 恢复；输出非诊断性错误并记录服务端日志（不回传栈/SQL）。
func recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				rid, _ := c.Get("request_id")
				log.Printf("panic request_id=%v: %v", rid, r)
				c.AbortWithStatusJSON(http.StatusInternalServerError, envelope{
					Error: &errBody{Code: "INTERNAL", Message: "internal error", RequestID: toString(rid)},
				})
			}
		}()
		c.Next()
	}
}

// securityHeaders 添加基础安全响应头。
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

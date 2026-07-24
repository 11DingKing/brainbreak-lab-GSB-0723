package httpapi

import (
	"github.com/gin-gonic/gin"
)

// NewRouter 构造路由。
func NewRouter(svc *Handlers) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), recovery(), requestID(), securityHeaders())

	r.GET("/healthz", svc.Health)

	v1 := r.Group("/v1")
	{
		v1.POST("/subjects", svc.CreateSubject)
		v1.GET("/subjects/:id", svc.GetSubject)
		v1.POST("/subjects/:id/withdraw", svc.WithdrawConsent)
		v1.DELETE("/subjects/:id", svc.DeleteSubject)

		v1.POST("/experiments", svc.CreateExperiment)
		v1.GET("/experiments/:id", svc.GetExperiment)
		v1.POST("/experiments/:id/events", svc.IngestEvents)
		v1.GET("/experiments/:id/results", svc.GetResult)
		v1.POST("/experiments/:id/recalc", svc.Recalc)
	}
	return r
}

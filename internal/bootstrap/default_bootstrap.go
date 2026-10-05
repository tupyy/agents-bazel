package bootstrap

import (
	"github.com/gin-gonic/gin"
	v2 "github.com/kubev2v/assisted-migration-agent/api/v2"
	"github.com/kubev2v/assisted-migration-agent/internal/config"
	v2Handlers "github.com/kubev2v/assisted-migration-agent/internal/handlers/v2"
	"github.com/kubev2v/assisted-migration-agent/internal/server"
	service "github.com/kubev2v/assisted-migration-agent/internal/services"
)

// New selects the public API. Private builds replace this file, retaining the
// shared builder and the entry point used by the CLI.
func New(cfg *config.Configuration) (*server.Server, func(), error) {
	return NewBuilder(cfg).WithGroup("/api/v2", publicAPIGroup).Build()
}

func publicAPIGroup(cfg *config.Configuration, svc *service.ServiceManager) (server.APIGroup, error) {
	swagger, err := v2.GetSwagger()
	if err != nil {
		return server.APIGroup{}, err
	}
	handler := v2Handlers.NewHandler(*cfg, svc)
	return server.APIGroup{
		Swagger: swagger,
		RegisterFn: func(router *gin.RouterGroup) {
			v2.RegisterHandlers(router, handler)
		},
	}, nil
}

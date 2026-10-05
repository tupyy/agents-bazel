package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	v2 "github.com/kubev2v/assisted-migration-agent/api/v2"
	"github.com/kubev2v/assisted-migration-agent/internal/config"
	v2Handlers "github.com/kubev2v/assisted-migration-agent/internal/handlers/v2"
	"github.com/kubev2v/assisted-migration-agent/internal/server"
	service "github.com/kubev2v/assisted-migration-agent/internal/services"
	"github.com/kubev2v/assisted-migration-agent/internal/store"
	"github.com/kubev2v/assisted-migration-agent/pkg/console"
	"github.com/kubev2v/assisted-migration-agent/pkg/crypto"
	"github.com/kubev2v/migration-planner/pkg/opa"
)

// New selects the public API. Private builds replace this file, retaining the
// shared builder and the entry point used by the CLI.
func New(cfg *config.Configuration) (*server.Server, func(), error) {
	return NewBuilder(cfg).WithGroup("/api/v2", initAPIGroup).Build()
}

func initAPIGroup(cfg *config.Configuration, pool *store.Pool, validator *opa.Validator) (server.APIGroup, func(), error) {
	jwt := ""
	if cfg.Auth.Enabled {
		data, err := os.ReadFile(cfg.Auth.JWTFilePath)
		if err != nil {
			return server.APIGroup{}, nil, fmt.Errorf("failed to read agent's jwt: %w", err)
		}
		if len(data) == 0 {
			return server.APIGroup{}, nil, errors.New("failed to read agent's jwt. the JWT is empty")
		}
		jwt = strings.TrimSpace(string(data))
	}

	consoleClient, err := console.NewConsoleClient(cfg.Console.URL, jwt)
	if err != nil {
		return server.APIGroup{}, nil, fmt.Errorf("failed to create console client: %w", err)
	}

	keyManager, err := crypto.NewKeyManager(cfg.Agent.DataFolder)
	if err != nil {
		return server.APIGroup{}, nil, fmt.Errorf("failed to initialize key manager: %w", err)
	}

	provider := service.NewServiceManager(
		service.WithConfig(cfg),
		service.WithPool(pool),
		service.WithOpaValidator(validator),
		service.WithConsoleClient(consoleClient),
		service.WithKeyManager(keyManager),
	)

	cleanup := func() {
		provider.Stop(context.Background())
	}

	if err := provider.Initialize(); err != nil {
		return server.APIGroup{}, cleanup, err
	}

	handler := v2Handlers.NewHandler(*cfg, provider)

	swagger, err := v2.GetSwagger()
	if err != nil {
		return server.APIGroup{}, cleanup, err
	}

	return server.APIGroup{
		Swagger: swagger,
		RegisterFn: func(router *gin.RouterGroup) {
			v2.RegisterHandlers(router, handler)
		},
	}, cleanup, nil
}

package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/kubev2v/assisted-migration-agent/internal/config"
	"github.com/kubev2v/assisted-migration-agent/internal/server"
	service "github.com/kubev2v/assisted-migration-agent/internal/services"
	"github.com/kubev2v/assisted-migration-agent/internal/store"
)

func testConfiguration(t *testing.T) *config.Configuration {
	t.Helper()
	dir := t.TempDir()
	policies := filepath.Join(dir, "policies")
	if err := os.Mkdir(policies, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(policies, "test.rego"), []byte("package io.konveyor.forklift.vmware\nconcerns := []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfigurationWithOptionsAndDefaults()
	cfg.Agent.DataFolder = dir
	cfg.Agent.OpaPoliciesFolder = policies
	cfg.Agent.ID = "550e8400-e29b-41d4-a716-446655440000"
	cfg.Agent.SourceID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	cfg.Agent.Mode = "disconnected"
	cfg.Auth.Enabled = false
	cfg.Console.URL = "http://127.0.0.1:9"
	cfg.Server.HTTPPort = 8000
	cfg.Server.ServerMode = "dev"
	return cfg
}

func TestBuilderRejectsInvalidGroupsBeforeStartup(t *testing.T) {
	factory := func(*config.Configuration, *service.ServiceManager) (server.APIGroup, error) {
		t.Fatal("invalid builder must not initialize API groups")
		return server.APIGroup{}, nil
	}
	for _, tc := range []struct {
		name string
		b    *Builder
		want string
	}{
		{"configuration", NewBuilder(nil), "configuration"},
		{"no groups", NewBuilder(&config.Configuration{}), "API group"},
		{"duplicate", NewBuilder(&config.Configuration{}).WithGroup("/api", factory).WithGroup("/api", factory), "duplicate"},
		{"prefix", NewBuilder(&config.Configuration{}).WithGroup("api", factory), "prefix"},
		{"factory", NewBuilder(&config.Configuration{}).WithGroup("/api", nil), "factory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cleanup, err := tc.b.Build()
			if err == nil || !strings.Contains(err.Error(), tc.want) || srv != nil || cleanup != nil {
				t.Fatalf("expected %s error before startup, got %v", tc.want, err)
			}
		})
	}
}

func TestBuilderRegistersGroupsAndOwnsCleanup(t *testing.T) {
	cfg := testConfiguration(t)
	cfg.Server.HTTPPort = 0
	var st *store.Store
	registered := 0
	factory := func(got *config.Configuration, svc *service.ServiceManager) (server.APIGroup, error) {
		if got != cfg || got.Server.HTTPPort != 8123 || svc.ConsoleService() == nil {
			t.Fatal("factory must receive configured, initialized services")
		}
		db, err := svc.Pool().Get(store.MainDatabaseID)
		if err != nil {
			return server.APIGroup{}, err
		}
		st, err = db.Store()
		return server.APIGroup{RegisterFn: func(router *gin.RouterGroup) {
			registered++
			router.GET("/health", func(c *gin.Context) { c.Status(200) })
		}}, err
	}
	srv, cleanup, err := NewBuilder(cfg).
		WithBeforeValidation(func(cfg *config.Configuration) error {
			cfg.Server.HTTPPort = 8123
			return nil
		}).
		WithGroup("/one", factory).
		WithGroup("/two", factory).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	if srv == nil || registered != 2 {
		t.Fatalf("expected both API groups to register, got %d", registered)
	}
	if err := st.VerifyConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if err := st.VerifyConnection(context.Background()); err == nil {
		t.Fatal("cleanup must close the database connection")
	}
}

func TestBuilderCleansUpOnFactoryFailure(t *testing.T) {
	want := errors.New("cannot assemble API")
	var st *store.Store
	srv, cleanup, err := NewBuilder(testConfiguration(t)).WithGroup("/api", func(_ *config.Configuration, svc *service.ServiceManager) (server.APIGroup, error) {
		db, err := svc.Pool().Get(store.MainDatabaseID)
		if err != nil {
			return server.APIGroup{}, err
		}
		st, err = db.Store()
		if err != nil {
			return server.APIGroup{}, err
		}
		return server.APIGroup{}, want
	}).Build()
	if !errors.Is(err, want) || srv != nil || cleanup != nil || st == nil {
		t.Fatalf("expected factory failure, got %v", err)
	}
	if err := st.VerifyConnection(context.Background()); err == nil {
		t.Fatal("factory failure must close the database connection")
	}
}

func TestDefaultBootstrapSelectsPublicAPI(t *testing.T) {
	group, err := publicAPIGroup(&config.Configuration{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if group.Swagger.Paths.Value("/credentials") == nil || group.Swagger.Paths.Value("/collector/rvtools") != nil {
		t.Fatal("default bootstrap must select the public schema")
	}
	router := gin.New()
	group.RegisterFn(router.Group("/api/v2"))
	for _, route := range router.Routes() {
		if route.Method == "GET" && route.Path == "/api/v2/credentials" {
			return
		}
	}
	t.Fatal("public routes were not registered")
}

package bootstrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/kubev2v/assisted-migration-agent/internal/config"
	"github.com/kubev2v/assisted-migration-agent/internal/server"
	"github.com/kubev2v/assisted-migration-agent/internal/store"
	"github.com/kubev2v/migration-planner/pkg/opa"
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
	factory := func(*config.Configuration, *store.Pool, *opa.Validator) (server.APIGroup, func(), error) {
		t.Fatal("invalid builder must not initialize API groups")

		return server.APIGroup{}, nil, nil
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

func TestBuilderSharesInputsAndOwnsCleanup(t *testing.T) {
	for _, failure := range []string{"none", "factory", "registration"} {
		t.Run(failure, func(t *testing.T) {
			cfg := testConfiguration(t)
			cfg.Server.HTTPPort = 0

			var sharedPool *store.Pool
			var sharedValidator *opa.Validator
			var st *store.Store
			var stopped []int
			created, registered := 0, 0
			want := errors.New("cannot assemble second API")

			factory := func(got *config.Configuration, pool *store.Pool, validator *opa.Validator) (server.APIGroup, func(), error) {
				if got != cfg || got.Server.HTTPPort != 8123 || pool == nil || validator == nil {
					t.Fatal("factory must receive configured, initialized inputs")
				}

				if created == 0 {
					sharedPool, sharedValidator = pool, validator
				} else if pool != sharedPool || validator != sharedValidator {
					t.Fatal("factories must share the same pool and validator")
				}

				db, err := pool.Get(store.MainDatabaseID)
				if err != nil {
					return server.APIGroup{}, nil, err
				}

				st, err = db.Store()
				if err != nil {
					return server.APIGroup{}, nil, err
				}

				created++
				id := created
				stop := func() {
					if err := st.VerifyConnection(context.Background()); err != nil {
						t.Errorf("pool closed before provider cleanup: %v", err)
					}

					stopped = append(stopped, id)
				}

				if id == 2 && failure == "factory" {
					return server.APIGroup{}, stop, want
				}

				if id == 2 && failure == "registration" {
					return server.APIGroup{}, stop, nil
				}

				group := server.APIGroup{RegisterFn: func(router *gin.RouterGroup) {
					registered++
					router.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
				}}

				return group, stop, nil
			}

			srv, cleanup, err := NewBuilder(cfg).
				WithBeforeValidation(func(cfg *config.Configuration) error {
					cfg.Server.HTTPPort = 8123

					return nil
				}).
				WithGroup("/one", factory).
				WithGroup("/two", factory).
				Build()

			if failure == "none" {
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(cleanup)
				if srv == nil || registered != 2 || len(stopped) != 0 {
					t.Fatal("expected both groups to register without premature cleanup")
				}

				cleanup()
				cleanup()
			} else if err == nil || srv != nil || cleanup != nil {
				t.Fatalf("expected startup failure, got %v", err)
			}

			if failure == "factory" && !errors.Is(err, want) {
				t.Fatalf("factory error was lost: %v", err)
			}

			if !slices.Equal(stopped, []int{2, 1}) {
				t.Fatalf("providers must stop once in reverse order, got %v", stopped)
			}

			if st == nil || st.VerifyConnection(context.Background()) == nil {
				t.Fatal("cleanup must close the database connection")
			}
		})
	}
}

func TestDefaultFactoryInitializesPublicProvider(t *testing.T) {
	cfg := testConfiguration(t)
	pool, err := InitPool(cfg)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(pool.Close)

	validator, err := InitOPA(cfg)
	if err != nil {
		t.Fatal(err)
	}

	group, cleanup, err := publicAPIGroup(cfg, pool, validator)
	if cleanup != nil {
		t.Cleanup(cleanup)
	}

	if err != nil {
		t.Fatal(err)
	}

	if group.Swagger.Paths.Value("/credentials") == nil || group.Swagger.Paths.Value("/collector/rvtools") != nil {
		t.Fatal("default bootstrap must select the public schema")
	}

	router := gin.New()
	group.RegisterFn(router.Group("/api/v2"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v2/agent", nil))

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "disconnected") {
		t.Fatalf("public provider was not initialized: %d: %s", response.Code, response.Body.String())
	}
}

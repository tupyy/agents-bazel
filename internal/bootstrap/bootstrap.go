package bootstrap

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kubev2v/migration-planner/pkg/opa"

	"github.com/kubev2v/assisted-migration-agent/internal/config"
	"github.com/kubev2v/assisted-migration-agent/internal/handlers"
	"github.com/kubev2v/assisted-migration-agent/internal/models"
	"github.com/kubev2v/assisted-migration-agent/internal/store"
	"github.com/kubev2v/assisted-migration-agent/internal/store/migrations"
	"github.com/kubev2v/assisted-migration-agent/pkg/console"
	"github.com/kubev2v/assisted-migration-agent/pkg/crypto"
)

func ValidateConfiguration(cfg *config.Configuration) error {
	if cfg.Agent.DataFolder == "" {
		return errors.New("data folder must be set")
	}

	if config.ServerModeType(cfg.Server.ServerMode) == config.ServerModeProd && cfg.Server.StaticsFolder == "" {
		return errors.New("statics folder must be set when server mode is production")
	}

	if cfg.Server.HTTPPort < 1 || cfg.Server.HTTPPort > 65535 {
		return fmt.Errorf("invalid http-port %d: must be between 1 and 65535", cfg.Server.HTTPPort)
	}

	if cfg.Auth.Enabled && cfg.Auth.JWTFilePath == "" {
		return errors.New("authentication-jwt-filepath must be set when authentication is enabled")
	}

	switch config.ServerModeType(cfg.Server.ServerMode) {
	case config.ServerModeProd, config.ServerModeDev:
	default:
		return fmt.Errorf("invalid server mode %q: must be %q or %q", cfg.Server.ServerMode, config.ServerModeProd, config.ServerModeDev)
	}

	if err := validateUUID(cfg.Agent.ID, "agent-id"); err != nil {
		return err
	}
	if err := validateUUID(cfg.Agent.SourceID, "source-id"); err != nil {
		return err
	}

	switch models.AgentMode(cfg.Agent.Mode) {
	case models.AgentModeConnected, models.AgentModeDisconnected:
	default:
		return fmt.Errorf("invalid mode %q: must be %q or %q", cfg.Agent.Mode, models.AgentModeConnected, models.AgentModeDisconnected)
	}

	return nil
}

func validateUUID(value, name string) error {
	if value == "" {
		return fmt.Errorf("%s cannot be empty", name)
	}
	if _, err := uuid.Parse(value); err != nil {
		return fmt.Errorf("%s must be a valid UUID: %w", name, err)
	}
	return nil
}

func InitOPA(cfg *config.Configuration) (*opa.Validator, error) {
	v, err := opa.NewValidatorFromDir(cfg.Agent.OpaPoliciesFolder)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize OPA validator: %w", err)
	}
	return v, nil
}

func InitPool(cfg *config.Configuration) (*store.Pool, error) {
	pool := store.NewPool(5 * time.Minute)

	agentPath := filepath.Join(cfg.Agent.DataFolder, "agent.duckdb")
	mainDB, err := pool.NewDatabase(store.MainDatabaseID, agentPath, time.Now(), store.EagerConnectionInitilization, 256, store.ReadWriteDatabase)
	if err != nil {
		return nil, fmt.Errorf("failed to create main database: %w", err)
	}

	if err := mainDB.Migrate(context.Background(), migrations.RunMain); err != nil {
		return nil, fmt.Errorf("failed to migrate main database: %w", err)
	}

	pool.Add(mainDB)
	zap.S().Infow("registered main database", "path", mainDB.Path)

	if err := cleanupStaleCollections(mainDB, cfg.Agent.DataFolder); err != nil {
		zap.S().Errorw("failed to cleanup stale collections", "error", err)
	}

	matches, _ := filepath.Glob(filepath.Join(cfg.Agent.DataFolder, "collection_*.duckdb"))
	for _, match := range matches {
		name := strings.TrimSuffix(filepath.Base(match), ".duckdb")
		tsStr := strings.TrimPrefix(name, "collection_")
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			zap.S().Warnw("skipping collection with unparseable timestamp", "file", match)
			continue
		}

		createdAt := time.Unix(ts, 0)
		hash := sha256.Sum256([]byte(match))
		id := hex.EncodeToString(hash[:])[:6]
		db, err := pool.NewDatabase(id, match, createdAt, store.LazyConnectionInitilization, 512, store.ReadWriteDatabase)
		if err != nil {
			zap.S().Warnw("skipping collection database", "file", match, "error", err)
			continue
		}

		if err := db.Migrate(context.Background(), func(ctx context.Context, db *sql.DB) error {
			return migrations.RunCollection(ctx, db, name)
		}); err != nil {
			zap.S().Errorw("failed to migrate collection database", "db_name", name, "error", err)
			_ = db.Close()
			continue
		}

		if err := db.Close(); err != nil {
			zap.S().Errorw("closing database", "db", db.ID, "error", err)
			continue
		}

		pool.Add(db)
		zap.S().Infow("registered collection database", "name", name, "path", match)
	}

	return pool, nil
}

func InitShared(cfg *config.Configuration) (*console.Client, *crypto.KeyManager, error) {
	jwt := ""
	if cfg.Auth.Enabled {
		data, err := os.ReadFile(cfg.Auth.JWTFilePath)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read agent's jwt: %w", err)
		}
		if len(data) == 0 {
			return nil, nil, errors.New("failed to read agent's jwt. the JWT is empty")
		}
		jwt = strings.TrimSpace(string(data))
	}

	consoleClient, err := console.NewConsoleClient(cfg.Console.URL, jwt)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create console client: %w", err)
	}

	keyManager, err := crypto.NewKeyManager(cfg.Agent.DataFolder)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize key manager: %w", err)
	}

	return consoleClient, keyManager, nil
}

func RegisterValidators() {
	if v, ok := binding.Validator.Engine().(*validator.Validate); ok {
		handlers.RegisterValidators(v)
	}
}

func cleanupStaleCollections(mainDB *store.Database, dataFolder string) error {
	st, err := mainDB.Store()
	if err != nil {
		return fmt.Errorf("failed to get main store: %w", err)
	}

	collections, err := st.Collection().List(context.Background())
	if err != nil {
		return fmt.Errorf("failed to list stale collections: %w", err)
	}

	for _, col := range collections {
		dbFile := filepath.Join(dataFolder, col.Database+".duckdb")
		if err := os.Remove(dbFile); err != nil && !os.IsNotExist(err) {
			zap.S().Warnw("failed to remove stale collection file", "file", dbFile, "error", err)
			continue
		}

		if err := st.Collection().Delete(context.Background(), col.Database); err != nil {
			zap.S().Warnw("failed to delete stale collection marker", "database", col.Database, "error", err)
			continue
		}

		zap.S().Infow("cleaned up stale collection", "database", col.Database, "state", col.State)
	}

	return nil
}

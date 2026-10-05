package bootstrap

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
	"github.com/kubev2v/assisted-migration-agent/internal/config"
	"github.com/kubev2v/assisted-migration-agent/internal/handlers"
	"github.com/kubev2v/assisted-migration-agent/internal/server"
	"github.com/kubev2v/assisted-migration-agent/internal/store"
	"github.com/kubev2v/assisted-migration-agent/internal/store/migrations"
	"github.com/kubev2v/migration-planner/pkg/opa"
	"go.uber.org/zap"
)

// APIGroupFactory initializes its provider and assembles its routes from shared inputs.
// Any returned cleanup runs before the shared pool closes, even when err is non-nil.
type APIGroupFactory func(*config.Configuration, *store.Pool, *opa.Validator) (server.APIGroup, func(), error)

type groupRegistration struct {
	prefix  string
	factory APIGroupFactory
}

// Builder shares startup and cleanup across builds without selecting an API.
// With methods only record options; Build allocates resources.
type Builder struct {
	cfg    *config.Configuration
	groups []groupRegistration
}

func NewBuilder(cfg *config.Configuration) *Builder {
	return &Builder{cfg: cfg}
}

func (b *Builder) WithGroup(prefix string, factory APIGroupFactory) *Builder {
	b.groups = append(b.groups, groupRegistration{prefix: prefix, factory: factory})

	return b
}

// Build initializes shared inputs and lets each factory initialize its provider.
// On success the caller owns cleanup; on failure resources are closed here.
func (b *Builder) Build() (*server.Server, func(), error) {
	if b.cfg == nil {
		return nil, nil, fmt.Errorf("configuration is required")
	}

	if len(b.groups) == 0 {
		return nil, nil, fmt.Errorf("at least one API group is required")
	}

	seen := make(map[string]bool, len(b.groups))
	for _, group := range b.groups {
		if !strings.HasPrefix(group.prefix, "/") || path.Clean(group.prefix) != group.prefix {
			return nil, nil, fmt.Errorf("invalid API group prefix %q", group.prefix)
		}
		if seen[group.prefix] {
			return nil, nil, fmt.Errorf("duplicate API group prefix %q", group.prefix)
		}
		if group.factory == nil {
			return nil, nil, fmt.Errorf("API group %q requires a factory", group.prefix)
		}
		seen[group.prefix] = true
	}

	opaValidator, err := b.initOPA(b.cfg)
	if err != nil {
		return nil, nil, err
	}

	pool, err := b.initPool(b.cfg)
	if err != nil {
		return nil, nil, err
	}

	var cleanups []func()
	var once sync.Once

	cleanup := func() {
		once.Do(func() {
			for i := len(cleanups) - 1; i >= 0; i-- {
				cleanups[i]()
			}

			pool.Close()
			zap.S().Info("providers and pool closed")
		})
	}

	built := false
	defer func() {
		if !built {
			cleanup()
		}
	}()

	if v, ok := binding.Validator.Engine().(*validator.Validate); ok {
		handlers.RegisterValidators(v)
	}

	groups := make(map[string]server.APIGroup, len(b.groups))
	for _, registration := range b.groups {
		group, stop, err := registration.factory(b.cfg, pool, opaValidator)
		if stop != nil {
			cleanups = append(cleanups, stop)
		}

		if err != nil {
			return nil, nil, fmt.Errorf("creating API group %q: %w", registration.prefix, err)
		}

		if group.RegisterFn == nil {
			return nil, nil, fmt.Errorf("API group %q requires route registration", registration.prefix)
		}

		groups[registration.prefix] = group
	}

	srv, err := server.NewServer(b.cfg, groups)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create HTTP server: %w", err)
	}

	built = true

	return srv, cleanup, nil
}

func (b *Builder) initOPA(cfg *config.Configuration) (*opa.Validator, error) {
	v, err := opa.NewValidatorFromDir(cfg.Agent.OpaPoliciesFolder)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize OPA validator: %w", err)
	}
	return v, nil
}

func (b *Builder) initPool(cfg *config.Configuration) (*store.Pool, error) {
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

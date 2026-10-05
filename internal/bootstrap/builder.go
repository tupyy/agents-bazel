package bootstrap

import (
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/kubev2v/assisted-migration-agent/internal/config"
	"github.com/kubev2v/assisted-migration-agent/internal/server"
	"github.com/kubev2v/assisted-migration-agent/internal/store"
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
	cfg              *config.Configuration
	beforeValidation func(*config.Configuration) error
	groups           []groupRegistration
}

func NewBuilder(cfg *config.Configuration) *Builder {
	return &Builder{cfg: cfg}
}

func (b *Builder) WithBeforeValidation(fn func(*config.Configuration) error) *Builder {
	b.beforeValidation = fn

	return b
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

	if b.beforeValidation != nil {
		if err := b.beforeValidation(b.cfg); err != nil {
			return nil, nil, err
		}
	}

	if err := ValidateConfiguration(b.cfg); err != nil {
		return nil, nil, err
	}

	opaValidator, err := InitOPA(b.cfg)
	if err != nil {
		return nil, nil, err
	}

	pool, err := InitPool(b.cfg)
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

	RegisterValidators()

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

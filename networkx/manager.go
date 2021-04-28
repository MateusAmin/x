package networkx

import (
	"context"
	"embed"

	"github.com/gobuffalo/pop/v5"
	"github.com/pkg/errors"

	"github.com/ory/x/logrusx"
	"github.com/ory/x/popx"
	"github.com/ory/x/sqlcon"
	"github.com/ory/x/tracing"
)

//go:embed migrations/sql/*.sql
var migrations embed.FS

type Manager struct {
	c *pop.Connection
	l *logrusx.Logger
	t *tracing.Tracer
}

func NewManager(
	c *pop.Connection,
	l *logrusx.Logger,
	t *tracing.Tracer,
) *Manager {
	return &Manager{
		c: c,
		l: l,
		t: t,
	}
}

func (m *Manager) Determine(ctx context.Context) (*Network, error) {
	var p Network
	c := m.c.WithContext(ctx)
	if err := sqlcon.HandleError(c.Q().Order("created_at ASC").First(&p)); err != nil {
		if errors.Is(err, sqlcon.ErrNoRows) {
			np := NewNetwork()
			if err := c.Create(np); err != nil {
				return nil, err
			}
			return np, nil
		}
		return nil, err
	}
	return &p, nil
}

func (m *Manager) MigrateUp(ctx context.Context) error {
	mm, err := popx.NewMigrationBox(migrations, popx.NewMigrator(m.c.WithContext(ctx), m.l, m.t, 0))
	if err != nil {
		return errors.WithStack(err)
	}

	return sqlcon.HandleError(mm.Up(ctx))
}
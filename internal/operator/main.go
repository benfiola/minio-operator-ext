package operator

import (
	"context"
	"io"
	"log/slog"

	"golang.org/x/sync/errgroup"
)

type main struct {
	Operator Operator
}

type Opts struct {
	Logger     *slog.Logger
	KubeConfig string
	ServerHost string
	ServerPort uint
}

func New(o *Opts) (*main, error) {
	l := o.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	op, err := NewOperator(&OperatorOpts{
		Logger:     l.With("name", "operator"),
		KubeConfig: o.KubeConfig,
	})
	if err != nil {
		return nil, err
	}

	return &main{
		Operator: op,
	}, nil
}

func (m *main) Run(ctx context.Context) error {
	g, _ := errgroup.WithContext(ctx)
	g.Go(func() error { return m.Operator.Run(ctx) })
	return g.Wait()
}

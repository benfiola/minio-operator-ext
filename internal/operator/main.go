package operator

import (
	"context"
	"io"
	"log/slog"

	"golang.org/x/sync/errgroup"
	"k8s.io/klog/v2"
)

// Main holds the components of the application
type Main struct {
	Operator Operator
}

// Opts define the options used to create a new instance of [Main]
type Opts struct {
	Logger     *slog.Logger
	KubeConfig string
}

// Creates a new instance of [Main] with the provided [Opts].
func New(o *Opts) (*Main, error) {
	l := o.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	klog.SetSlogLogger(l.With("name", "klog"))
	op, err := NewOperator(&OperatorOpts{
		Logger:     l.With("name", "operator"),
		KubeConfig: o.KubeConfig,
	})
	if err != nil {
		return nil, err
	}

	return &Main{
		Operator: op,
	}, nil
}

// Runs the application.
// Blocks until one of the components fail with an error
func (m *Main) Run(ctx context.Context) error {
	g, _ := errgroup.WithContext(ctx)
	g.Go(func() error { return m.Operator.Run(ctx) })
	return g.Wait()
}

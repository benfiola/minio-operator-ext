package operator

import (
	"io"
	"log/slog"
	"time"
)

type operator struct {
	logger *slog.Logger
}

type OperatorOpts struct {
	Logger *slog.Logger
}

func NewOperator(o *OperatorOpts) (*operator, error) {
	l := o.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &operator{
		logger: l,
	}, nil
}

func (o *operator) Health() error {
	return nil
}
func (o *operator) Run() error {
	o.logger.Info("starting operator")
	for {
		time.Sleep(5 * time.Second)
	}
	return nil
}

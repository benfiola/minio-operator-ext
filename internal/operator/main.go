/*
Copyright (C) 2025  Ben Fiola

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/
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
	Logger                 *slog.Logger
	KubeConfig             string
	MinioOperatorNamespace string
}

// Creates a new instance of [Main] with the provided [Opts].
func New(o *Opts) (*Main, error) {
	l := o.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	klog.SetSlogLogger(l.With("name", "klog"))
	op, err := NewOperator(&OperatorOpts{
		Logger:                 l.With("name", "operator"),
		KubeConfig:             o.KubeConfig,
		MinioOperatorNamespace: o.MinioOperatorNamespace,
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

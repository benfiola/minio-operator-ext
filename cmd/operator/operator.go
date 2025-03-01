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
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/benfiola/minio-operator-ext/internal"
	"github.com/benfiola/minio-operator-ext/internal/operator"
	"github.com/urfave/cli/v2"
)

// Configures logging for the application.
// Accepts a logging level 'error' | 'warn' | 'info' | 'debug'
func configureLogging(ls string) (*slog.Logger, error) {
	if ls == "" {
		ls = "info"
	}
	var l slog.Level
	switch ls {
	case "error":
		l = slog.LevelError
	case "warn":
		l = slog.LevelWarn
	case "info":
		l = slog.LevelInfo
	case "debug":
		l = slog.LevelDebug
	default:
		return nil, fmt.Errorf("unrecognized log level %s", ls)
	}

	opts := &slog.HandlerOptions{
		Level: l,
	}
	handler := slog.NewTextHandler(os.Stderr, opts)
	logger := slog.New(handler)
	return logger, nil
}

// Used as a key to the urfave/cli context to store the application-level logger.
type ContextLogger struct{}

func main() {
	err := (&cli.App{
		Usage: "the minio-operator-ext cli",
		Before: func(c *cli.Context) error {
			logger, err := configureLogging(c.String("log-level"))
			if err != nil {
				return err
			}
			c.Context = context.WithValue(c.Context, ContextLogger{}, logger)
			return nil
		},
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "log-level",
				Usage:   "logging verbosity level",
				EnvVars: []string{"MINIO_OPERATOR_EXT_LOG_LEVEL"},
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "run",
				Usage: "start operator",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "kube-config",
						Usage:   "path to kubeconfig file",
						EnvVars: []string{"MINIO_OPERATOR_EXT_KUBE_CONFIG"},
						Value:   "",
					},
				},
				Action: func(c *cli.Context) error {
					l, ok := c.Context.Value(ContextLogger{}).(*slog.Logger)
					if !ok {
						return fmt.Errorf("logger not attached to context")
					}

					s, err := operator.New(&operator.Opts{
						KubeConfig: c.String("kube-config"),
						Logger:     l,
					})
					if err != nil {
						return err
					}

					return s.Run(c.Context)
				},
			},
			{
				Name:  "version",
				Usage: "prints the operator version",
				Action: func(c *cli.Context) error {
					fmt.Fprintf(c.App.Writer, "%s", internal.GetOperatorVersion())
					return nil
				},
			},
		},
	}).Run(os.Args)
	code := 0
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		code = 1
	}
	os.Exit(code)
}

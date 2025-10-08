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
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"time"

	v1 "github.com/benfiola/minio-operator-ext/pkg/api/bfiola.dev/v1"
	"github.com/go-logr/logr"
	"github.com/minio/madmin-go/v3"
	minioclient "github.com/minio/minio-go/v7"
	miniov2 "github.com/minio/operator/pkg/apis/minio.min.io/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	finalizer               = "bfiola.dev/minio-operator-ext"
	AccessKeyFieldAccessKey = "accessKey"
	AccessKeyFieldSecretKey = "secretKey"
)

// Returns a pointer to [v].
// This primarily exists to simplify passing pointers to literals.
func ptr[T any](v T) *T {
	return &v
}

// Returns true if the error is an [minioclient.ErrorResponse] and its code matches that of [code].
func isMinioErrorCode(err error, code string) bool {
	merr, ok := err.(minioclient.ErrorResponse)
	return ok && merr.Code == code
}

// Ignores the error if the error is an [minioclient.ErrorResponse] and its code matches that of [code].
func ignoreMinioErrorCode(err error, code string) error {
	if isMinioErrorCode(err, code) {
		return nil
	}
	return err
}

// Returns true if the error is an [madmin.ErrorResponse] and its code matches that of [code].
func isMadminErrorCode(err error, code string) bool {
	merr, ok := err.(madmin.ErrorResponse)
	return ok && merr.Code == code
}

// Ignores the error if the error is an [madmin.ErrorResponse] and its code matches that of [code].
func ignoreMadminErrorCode(err error, code string) error {
	if isMadminErrorCode(err, code) {
		return nil
	}
	return err
}

// Operator is the public interface for the operator implementation
type Operator interface {
	Health() error
	Run(ctx context.Context) error
}

// operator manages all of the crd controllers
type operator struct {
	manager manager.Manager
	logger  *slog.Logger
}

// OperatorOpts defines the options used to construct a new [operator]
type OperatorOpts struct {
	KubeConfig             string
	Logger                 *slog.Logger
	MinioOperatorNamespace string
	SyncInterval           time.Duration
}

// Creates a new operator with the provided [OperatorOpts].
func NewOperator(o *OperatorOpts) (*operator, error) {
	l := o.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	c, err := clientcmd.BuildConfigFromFlags("", o.KubeConfig)
	if err != nil {
		return nil, err
	}

	mon := o.MinioOperatorNamespace
	if mon == "" {
		d, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err == nil {
			mon = string(d)
		}
	}
	if mon == "" {
		return nil, fmt.Errorf("minio operator namespace is not defined")
	}

	si := o.SyncInterval
	if si == 0 {
		si = 60 * time.Second
	}

	s := runtime.NewScheme()
	err = v1.AddToScheme(s)
	if err != nil {
		return nil, err
	}
	err = miniov2.AddToScheme(s)
	if err != nil {
		return nil, err
	}
	err = kscheme.AddToScheme(s)
	if err != nil {
		return nil, err
	}

	lfsl := logr.FromSlogHandler(l.Handler())
	log.SetLogger(lfsl)
	m, err := manager.New(c, manager.Options{
		Client:                 client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.ConfigMap{}, &corev1.Secret{}, &corev1.Service{}, &miniov2.Tenant{}}}},
		Controller:             config.Controller{SkipNameValidation: ptr(true)},
		HealthProbeBindAddress: ":8888",
		Logger:                 lfsl,
		Scheme:                 s,
	})
	if err != nil {
		return nil, err
	}
	err = m.AddHealthzCheck("healthz", healthz.Ping)
	if err != nil {
		return nil, err
	}
	err = m.AddReadyzCheck("readyz", healthz.Ping)
	if err != nil {
		return nil, err
	}

	rs := []reconciler{
		&minioBucketReconciler{minioOperatorNamespace: mon, syncInterval: si},
		&minioGroupReconciler{minioOperatorNamespace: mon, syncInterval: si},
		&minioGroupBindingReconciler{minioOperatorNamespace: mon, syncInterval: si},
		&minioPolicyReconciler{minioOperatorNamespace: mon, syncInterval: si},
		&minioPolicyBindingReconciler{minioOperatorNamespace: mon, syncInterval: si},
		&minioUserReconciler{minioOperatorNamespace: mon, syncInterval: si},
		&minioAccessKeyReconciler{minioOperatorNamespace: mon, syncInterval: si},
	}
	for _, r := range rs {
		err = r.register(m)
		if err != nil {
			return nil, err
		}
	}

	return &operator{
		logger:  l,
		manager: m,
	}, err
}

// Performs a health check for the given [operator],
// Returns an error if the [operator] is unhealthy.
func (o *operator) Health() error {
	return nil
}

// Starts the [operator].
// Runs until terminated or if an error is thrown.
func (o *operator) Run(ctx context.Context) error {
	o.logger.Info("starting operator")
	return o.manager.Start(ctx)
}

// reconciler is the common interface implemented by all CRD reconcilers in this package
type reconciler interface {
	reconcile.Reconciler
	register(m manager.Manager) error
}

// Converts src to dst by marshaling through XML and JSON.
// Marshalling through JSON clears any XML-specific fields (e.g., XMLName)
// Returns an error on failure.
func convertViaXML[T any](src any, dst *T) error {
	data, err := xml.Marshal(src)
	if err != nil {
		return err
	}
	var tmpv T
	tmp := &tmpv
	err = xml.Unmarshal(data, tmp)
	if err != nil {
		return err
	}
	data, err = json.Marshal(tmp)
	if err != nil {
		return err
	}
	err = json.Unmarshal(data, dst)
	if err != nil {
		return err
	}
	return nil
}

// Returns the value of a pointer to T.
// If the value is nil, returns the zero value of the type.
// This is used to facilitate equality checks where nil and the type's zero value are treated as equivalent.
func nilToEmpty[T any](v *T) T {
	if v == nil {
		rv := reflect.ValueOf(v)
		zv := reflect.Zero(rv.Type().Elem()).Interface().(T)
		return zv
	}
	return *v
}

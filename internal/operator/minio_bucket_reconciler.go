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
	"reflect"
	"time"

	v1 "github.com/benfiola/minio-operator-ext/pkg/api/bfiola.dev/v1"
	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	minioclient "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// minioBucketReconciler reconciles [v1.MinioBucket] resources
type minioBucketReconciler struct {
	client.Client
	logger                 logr.Logger
	minioOperatorNamespace string
	syncInterval           time.Duration
}

// Builds a controller with a [minioBucketReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioBucketReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioBucket{}).Build(r)
	if err != nil {
		return err
	}
	r.logger = ctrl.GetLogger()
	return nil
}

// +kubebuilder:rbac:groups=bfiola.dev,resources=miniobuckets,verbs=get;list;update;watch

// Reconciles a [reconcile.Request] associated with a [v1.MinioBucket].
// Returns a error if reconciliation fails.
func (r *minioBucketReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	b := &v1.MinioBucket{}
	err := r.Get(ctx, req.NamespacedName, b)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	success := func() (reconcile.Result, error) { return reconcile.Result{RequeueAfter: r.syncInterval}, nil }
	failure := func(err error) (reconcile.Result, error) { return reconcile.Result{}, err }

	if !b.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")
		if b.Status.CurrentSpec != nil {
			l.Info("delete bucket (status set)")

			l.Info("get tenant client")
			tr := b.Status.CurrentSpec.TenantRef.SetDefaultNamespace(b.GetNamespace())
			mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
			if err != nil {
				return failure(err)
			}
			mtc, err := mtci.GetClient(ctx)
			if err != nil {
				return failure(err)
			}

			l.Info("delete minio bucket")
			dp := b.Status.CurrentSpec.DeletionPolicy
			if b.Status.CurrentSpec.DeletionPolicy == "" {
				dp = v1.MinioBucketDeletionPolicyIfEmpty
			}
			err = mtc.RemoveBucketWithOptions(ctx, b.Status.CurrentSpec.Name, minioclient.RemoveBucketOptions{ForceDelete: dp == v1.MinioBucketDeletionPolicyAlways})
			err = ignoreMinioErrorCode(err, "NoSuchBucket")
			if err != nil {
				return failure(err)
			}

			l.Info("clear status")
			b.Status.CurrentSpec = nil
			err = r.Update(ctx, b)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(b, finalizer)
		err = r.Update(ctx, b)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if !controllerutil.ContainsFinalizer(b, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(b, finalizer)
		err = r.Update(ctx, b)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if b.Status.CurrentSpec != nil {
		l.Info("check for bucket change")

		if b.Spec.Migrate {
			l.Info("remove migrate spec field")
			b.Spec.Migrate = false
			err = r.Update(ctx, b)
			if err != nil {
				return failure(err)
			}
		}

		l.Info("get tenant client")
		tr := b.Status.CurrentSpec.TenantRef.SetDefaultNamespace(b.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtc, err := mtci.GetClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("check if minio bucket exists")
		be, err := mtc.BucketExists(ctx, b.Status.CurrentSpec.Name)
		if err != nil {
			return failure(err)
		}
		if !be {
			l.Info("clear status (minio bucket no longer exists)")
			b.Status.CurrentSpec = nil
			err := r.Update(ctx, b)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("get bucket versioning configuration")
		bvc, err := mtc.GetBucketVersioning(ctx, b.Status.CurrentSpec.Name)
		if err != nil {
			return failure(err)
		}
		mbvc := &v1.MinioBucketVersioningConfiguration{}
		err = convertViaXML(bvc, mbvc)
		if err != nil {
			return failure(err)
		}
		if !cmp.Equal(nilToEmpty(mbvc), nilToEmpty(b.Status.CurrentSpec.Versioning), cmpopts.EquateEmpty()) {
			l.Info("bucket versioning configuration changed (status and remote differ)")

			l.Info("set bucket versioning configuration")
			bvc = minioclient.BucketVersioningConfiguration{}
			err = convertViaXML(b.Status.CurrentSpec.Versioning, &bvc)
			if err != nil {
				return failure(err)
			}
			err = mtc.SetBucketVersioning(ctx, b.Status.CurrentSpec.Name, bvc)
			if err != nil {
				return failure(err)
			}
		}
		if !reflect.DeepEqual(b.Spec.Versioning, b.Status.CurrentSpec.Versioning) {
			l.Info("bucket versioning configuration changed (status and spec differ)")

			l.Info("set bucket versioning configuration")
			bvc = minioclient.BucketVersioningConfiguration{}
			err = convertViaXML(b.Spec.Versioning, &bvc)
			if err != nil {
				return failure(err)
			}
			err = mtc.SetBucketVersioning(ctx, b.Status.CurrentSpec.Name, bvc)
			if err != nil {
				return failure(err)
			}

			l.Info("set status")
			b.Status.CurrentSpec.Versioning = b.Spec.Versioning
			err = r.Update(ctx, b)
			if err != nil {
				return failure(err)
			}
		}

		l.Info("get bucket lifecycle configuration")
		blc, err := mtc.GetBucketLifecycle(ctx, b.Status.CurrentSpec.Name)
		err = ignoreMinioErrorCode(err, "NoSuchLifecycleConfiguration")
		if err != nil {
			return failure(err)
		}
		if blc == nil {
			blc = lifecycle.NewConfiguration()
		}
		mblc := &v1.MinioBucketLifecycleConfiguration{}
		err = convertViaXML(blc, mblc)
		if err != nil {
			return failure(err)
		}
		if !cmp.Equal(nilToEmpty(mblc), nilToEmpty(b.Status.CurrentSpec.Lifecycle), cmpopts.EquateEmpty()) {
			l.Info("bucket lifecycle configuration changed (status and remote differ)")

			l.Info("set bucket lifecycle configuration")
			blc = &lifecycle.Configuration{}
			err = convertViaXML(b.Status.CurrentSpec.Lifecycle, blc)
			if err != nil {
				return failure(err)
			}
			err = mtc.SetBucketLifecycle(ctx, b.Status.CurrentSpec.Name, blc)
			if err != nil {
				return failure(err)
			}
		}
		if !reflect.DeepEqual(b.Spec.Lifecycle, b.Status.CurrentSpec.Lifecycle) {
			l.Info("bucket lifecycle configuration changed (status and spec differ)")

			l.Info("set bucket lifecycle configuration")
			blc = &lifecycle.Configuration{}
			err = convertViaXML(b.Spec.Lifecycle, blc)
			if err != nil {
				return failure(err)
			}
			err = mtc.SetBucketLifecycle(ctx, b.Status.CurrentSpec.Name, blc)
			if err != nil {
				return failure(err)
			}

			l.Info("set status")
			b.Status.CurrentSpec.Lifecycle = b.Spec.Lifecycle
			err = r.Update(ctx, b)
			if err != nil {
				return failure(err)
			}
		}

		if b.Status.CurrentSpec.DeletionPolicy != b.Spec.DeletionPolicy {
			l.Info("update bucket (deletion policy changed)")

			l.Info("set status")
			b.Status.CurrentSpec.DeletionPolicy = b.Spec.DeletionPolicy
			err = r.Update(ctx, b)
			if err != nil {
				return failure(err)
			}
		}

		return success()
	}

	if b.Status.CurrentSpec == nil {
		l.Info("create bucket (status unset)")

		l.Info("get tenant client")
		tr := b.Spec.TenantRef.SetDefaultNamespace(b.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtc, err := mtci.GetClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("create minio bucket")
		err = mtc.MakeBucket(ctx, b.Spec.Name, minioclient.MakeBucketOptions{})
		if b.Spec.Migrate {
			err = ignoreMinioErrorCode(err, "BucketAlreadyOwnedByYou")
		}
		if err != nil {
			return failure(err)
		}

		l.Info("set bucket versioning")
		if b.Spec.Versioning != nil {
			bvc := minioclient.BucketVersioningConfiguration{}
			err = convertViaXML(b.Spec.Versioning, &bvc)
			if err != nil {
				return failure(err)
			}
			err = mtc.SetBucketVersioning(ctx, b.Spec.Name, bvc)
			if err != nil {
				return failure(err)
			}
		}

		l.Info("set bucket lifecycle")
		if b.Spec.Lifecycle != nil {
			blc := &lifecycle.Configuration{}
			err = convertViaXML(b.Spec.Lifecycle, blc)
			if err != nil {
				return failure(err)
			}
			err = mtc.SetBucketLifecycle(ctx, b.Spec.Name, blc)
			if err != nil {
				return failure(err)
			}
		}

		l.Info("set status")
		b.Spec.Migrate = false
		b.Status.CurrentSpec = &b.Spec
		err = r.Update(ctx, b)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	return success()
}

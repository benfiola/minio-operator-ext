<<<<<<< HEAD
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
=======
>>>>>>> 38bcac6 (Split internal/operator/operator.go into multiple files)
package operator

import (
	"context"
	"fmt"
	"time"

	v1 "github.com/benfiola/minio-operator-ext/pkg/api/bfiola.dev/v1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// minioUserReconciler reconciles [v1.MinioUser] resources
type minioUserReconciler struct {
	client.Client
	logger                 logr.Logger
	minioOperatorNamespace string
	syncInterval           time.Duration
}

// Builds a controller with a [minioUserReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioUserReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioUser{}).Build(r)
	if err != nil {
		return err
	}
	r.logger = ctrl.GetLogger()
	return nil
}

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get
// +kubebuilder:rbac:groups=bfiola.dev,resources=miniousers,verbs=get;list;update;watch

// Reconciles a [reconcile.Request] associated with a [v1.MinioUser].
// Returns a error if reconciliation fails.
func (r *minioUserReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	u := &v1.MinioUser{}
	err := r.Get(ctx, req.NamespacedName, u)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	success := func() (reconcile.Result, error) { return reconcile.Result{RequeueAfter: r.syncInterval}, nil }
	failure := func(err error) (reconcile.Result, error) { return reconcile.Result{}, err }

	if !u.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if u.Status.CurrentSpec != nil {
			l.Info("delete user (status set)")

			l.Info("get tenant admin client")
			tr := u.Status.CurrentSpec.TenantRef.SetDefaultNamespace(u.GetNamespace())
			mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
			if err != nil {
				return failure(err)
			}
			mtac, err := mtci.GetAdminClient(ctx)
			if err != nil {
				return failure(err)
			}

			l.Info("delete minio user")
			err = mtac.RemoveUser(ctx, u.Status.CurrentSpec.AccessKey)
			err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchUser")
			if err != nil {
				return failure(err)
			}

			l.Info("clear status")
			u.Status.CurrentSpec = nil
			err = r.Update(ctx, u)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(u, finalizer)
		err = r.Update(ctx, u)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if !controllerutil.ContainsFinalizer(u, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(u, finalizer)
		err = r.Update(ctx, u)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if u.Status.CurrentSpec != nil {
		l.Info("check for user change")

		if u.Spec.Migrate {
			l.Info("remove migrate spec field")
			u.Spec.Migrate = false
			err = r.Update(ctx, u)
			if err != nil {
				return failure(err)
			}
		}

		l.Info("get tenant admin client")
		tr := u.Status.CurrentSpec.TenantRef.SetDefaultNamespace(u.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("get minio user")
		_, err = mtac.GetUserInfo(ctx, u.Status.CurrentSpec.AccessKey)
		e := !isMadminErrorCode(err, "XMinioAdminNoSuchUser")
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchUser")
		if err != nil {
			return failure(err)
		}
		if !e {
			l.Info("clear status (minio user no longer exists)")
			u.Status.CurrentSpec = nil
			err = r.Update(ctx, u)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("get secret from secret key ref")
		usrr := u.Status.CurrentSpec.SecretKeyRef.SetDefaultNamespace(req.Namespace)
		us := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: usrr.Name, Namespace: usrr.Namespace}, us)
		if err != nil {
			return failure(err)
		}
		if us.GetResourceVersion() != u.Status.CurrentSecretKeyRefResourceVersion {
			l.Info("update user (secret key ref change)")
			usk := string(us.Data[u.Status.CurrentSpec.SecretKeyRef.Key])
			err = mtac.AddUser(ctx, u.Status.CurrentSpec.AccessKey, usk)
			if err != nil {
				return failure(err)
			}

			l.Info("set status")
			u.Status.CurrentSecretKeyRefResourceVersion = us.GetResourceVersion()
			err = r.Update(ctx, u)
			if err != nil {
				return failure(err)
			}

			return success()
		}
	}

	if u.Status.CurrentSpec == nil {
		l.Info("create user (status unset)")

		l.Info("get tenant admin client")
		tr := u.Spec.TenantRef.SetDefaultNamespace(u.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("get minio user")
		_, err = mtac.GetUserInfo(ctx, u.Spec.AccessKey)
		exists := err == nil
		if exists && !u.Spec.Migrate {
			err = fmt.Errorf("user %s already exists", u.Spec.AccessKey)
		}
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchUser")
		if err != nil {
			return failure(err)
		}

		l.Info("get secret from secret key ref")
		usrr := u.Spec.SecretKeyRef.SetDefaultNamespace(req.Namespace)
		us := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: usrr.Name, Namespace: usrr.Namespace}, us)
		if err != nil {
			return failure(err)
		}
		usk := string(us.Data[u.Spec.SecretKeyRef.Key])

		l.Info("create minio user")
		err = mtac.AddUser(ctx, u.Spec.AccessKey, usk)
		if err != nil {
			return failure(err)
		}

		l.Info("set status")
		u.Spec.Migrate = false
		u.Status.CurrentSpec = &u.Spec
		u.Status.CurrentSecretKeyRefResourceVersion = us.GetResourceVersion()
		err = r.Update(ctx, u)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	return success()
}

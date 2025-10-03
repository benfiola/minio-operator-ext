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
	"fmt"
	"time"

	v1 "github.com/benfiola/minio-operator-ext/pkg/api/bfiola.dev/v1"
	"github.com/go-logr/logr"
	"github.com/minio/madmin-go/v3"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// minioServiceAccountReconciler reconciles [v1.MinioServiceAccount] resources
type minioServiceAccountReconciler struct {
	client.Client
	logger                 logr.Logger
	minioOperatorNamespace string
	syncInterval           time.Duration
}

// Builds a controller with a [minioServiceAccountReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioServiceAccountReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioServiceAccount{}).Build(r)
	if err != nil {
		return err
	}
	r.logger = ctrl.GetLogger()
	return nil
}

// +kubebuilder:rbac:groups=bfiola.dev,resources=minioserviceaccounts,verbs=get;list;update;watch

// Reconciles a [reconcile.Request] associated with a [v1.MinioServiceAccount].
// Returns a error if reconciliation fails.
func (r *minioServiceAccountReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	sa := &v1.MinioServiceAccount{}
	err := r.Get(ctx, req.NamespacedName, sa)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !sa.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if sa.Status.CurrentSpec != nil {
			l.Info("delete service account (status set)")

			l.Info("get tenant admin client")
			mtac, err := r.getAdminClient(ctx, sa.Status.CurrentSpec.TenantRef.Name, sa.GetNamespace())
			if err != nil {
				return failedReconcilliation(err)
			}

			l.Info("delete minio service account")
			err = mtac.DeleteServiceAccount(ctx, sa.Status.CurrentSpec.AccessKey)
			err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
			if err != nil {
				return failedReconcilliation(err)
			}

			l.Info("clear status")
			sa.Status.CurrentSpec = nil
			err = r.Update(ctx, sa)
			if err != nil {
				return failedReconcilliation(err)
			}

			return successfulReconcilliation(r.syncInterval)
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(sa, finalizer)
		err = r.Update(ctx, sa)
		if err != nil {
			return failedReconcilliation(err)
		}

		return successfulReconcilliation(r.syncInterval)
	}

	if !controllerutil.ContainsFinalizer(sa, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(sa, finalizer)
		err = r.Update(ctx, sa)
		if err != nil {
			return failedReconcilliation(err)
		}

		return successfulReconcilliation(r.syncInterval)
	}

	if sa.Status.CurrentSpec != nil {
		l.Info("check for service account change")

		if sa.Spec.Migrate {
			l.Info("remove migrate spec field")
			sa.Spec.Migrate = false
			err = r.Update(ctx, sa)
			if err != nil {
				return failedReconcilliation(err)
			}
		}

		l.Info("get tenant admin client")
		mtac, err := r.getAdminClient(ctx, sa.Status.CurrentSpec.TenantRef.Name, sa.GetNamespace())
		if err != nil {
			return failedReconcilliation(err)
		}

		l.Info("get minio service account")
		_, err = mtac.InfoServiceAccount(ctx, sa.Status.CurrentSpec.AccessKey)
		e := !isMadminErrorCode(err, "XMinioInvalidIAMCredentials")
		err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
		if err != nil {
			return failedReconcilliation(err)
		}
		if !e {
			l.Info("clear status (minio service account no longer exists)")
			sa.Status.CurrentSpec = nil
			err = r.Update(ctx, sa)
			if err != nil {
				return failedReconcilliation(err)
			}

			return successfulReconcilliation(r.syncInterval)
		}

		// TODO: update service account, see
		// mtac.UpdateServiceAccount(ctx, sa.Status.CurrentSpec.AccessKey, madmin.UpdateServiceAccountReq{})
	}

	if sa.Status.CurrentSpec == nil {
		l.Info("create service account (status unset)")

		l.Info("get tenant admin client")
		mtac, err := r.getAdminClient(ctx, sa.Spec.TenantRef.Name, sa.GetNamespace())
		if err != nil {
			return failedReconcilliation(err)
		}

		if sa.Spec.AccessKey != "" {
			l.Info("get minio service account")
			_, err = mtac.InfoServiceAccount(ctx, sa.Spec.AccessKey)
			exists := err == nil
			if exists && !sa.Spec.Migrate {
				err = fmt.Errorf("service account %s already exists", sa.Spec.AccessKey)
			}
			err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
			if err != nil {
				return failedReconcilliation(err)
			}
		}

		l.Info("create minio service account")
		creds, err := mtac.AddServiceAccount(ctx, madmin.AddServiceAccountReq{
			TargetUser:  sa.Spec.TargetUser,
			AccessKey:   sa.Spec.AccessKey,
			SecretKey:   sa.Spec.SecretKey,
			Name:        sa.Spec.Name,
			Description: sa.Spec.Description,

			// TODO: implement
			// Policy:      nil,
			// Expiration:  sa.Spec.Expiration,
		})
		if err != nil {
			return failedReconcilliation(err)
		}

		l.Info("set status")
		sa.Spec.Migrate = false
		sa.Status.CurrentSpec = &sa.Spec
		sa.Status.CurrentSpec.AccessKey = creds.AccessKey
		err = r.Update(ctx, sa)
		if err != nil {
			return failedReconcilliation(err)
		}

		return successfulReconcilliation(r.syncInterval)
	}

	// TODO:
	// Create secret with credentials
	// sa.Spec.TargetSecretName

	return successfulReconcilliation(r.syncInterval)
}

func (r *minioServiceAccountReconciler) getAdminClient(ctx context.Context, name, namespace string) (*madmin.AdminClient, error) {
	mtci, err := getMinioTenantClientInfo(ctx, r, v1.ResourceRef{
		Name:      name,
		Namespace: namespace,
	}, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
	if err != nil {
		return nil, err
	}
	mtac, err := mtci.GetAdminClient(ctx)
	if err != nil {
		return nil, err
	}
	return mtac, nil
}

func successfulReconcilliation(duration time.Duration) (reconcile.Result, error) {
	return reconcile.Result{RequeueAfter: duration}, nil
}

func failedReconcilliation(err error) (reconcile.Result, error) {
	return reconcile.Result{}, err
}

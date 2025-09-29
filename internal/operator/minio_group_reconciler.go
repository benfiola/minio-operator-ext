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

// minioGroupReconciler reconciles [v1.MinioGroup] resources
type minioGroupReconciler struct {
	client.Client
	logger                 logr.Logger
	minioOperatorNamespace string
	syncInterval           time.Duration
}

// Builds a controller with a [minioGroupReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioGroupReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioGroup{}).Build(r)
	if err != nil {
		return err
	}
	r.logger = ctrl.GetLogger()
	return nil
}

// +kubebuilder:rbac:groups=bfiola.dev,resources=miniogroups,verbs=get;list;update;watch

// Reconciles a [reconcile.Request] associated with a [v1.MinioGroup].
// Returns a error if reconciliation fails.
func (r *minioGroupReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	g := &v1.MinioGroup{}
	err := r.Get(ctx, req.NamespacedName, g)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	success := func() (reconcile.Result, error) { return reconcile.Result{RequeueAfter: r.syncInterval}, nil }
	failure := func(err error) (reconcile.Result, error) { return reconcile.Result{}, err }
	if !g.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if g.Status.CurrentSpec != nil {
			l.Info("delete group (status set)")

			l.Info("get tenant admin client")
			tr := g.Status.CurrentSpec.TenantRef.SetDefaultNamespace(g.GetNamespace())
			mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
			if err != nil {
				return failure(err)
			}
			mtac, err := mtci.GetAdminClient(ctx)
			if err != nil {
				return failure(err)
			}

			l.Info("delete minio group")
			err = mtac.UpdateGroupMembers(ctx, madmin.GroupAddRemove{
				Group:    g.Status.CurrentSpec.Name,
				IsRemove: true,
			})
			err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchGroup")
			if err != nil {
				return failure(err)
			}

			l.Info("clear status")
			g.Status.CurrentSpec = nil
			err = r.Update(ctx, g)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(g, finalizer)
		err = r.Update(ctx, g)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if !controllerutil.ContainsFinalizer(g, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(g, finalizer)
		err = r.Update(ctx, g)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if g.Status.CurrentSpec != nil {
		l.Info("check for group change")

		if g.Spec.Migrate {
			l.Info("remove migrate spec field")
			g.Spec.Migrate = false
			err = r.Update(ctx, g)
			if err != nil {
				return failure(err)
			}
		}

		l.Info("get tenant admin client")
		tr := g.Status.CurrentSpec.TenantRef.SetDefaultNamespace(g.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("get minio group description")
		_, err = mtac.GetGroupDescription(ctx, g.Status.CurrentSpec.Name)
		e := !isMadminErrorCode(err, "XMinioAdminNoSuchGroup")
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchGroup")
		if err != nil {
			return failure(err)
		}
		if !e {
			l.Info("clear status (minio group no longer exists)")
			g.Status.CurrentSpec = nil
			err = r.Update(ctx, g)
			if err != nil {
				return failure(err)
			}

			return success()
		}
	}

	if g.Status.CurrentSpec == nil {
		l.Info("create group (status unset)")

		l.Info("get tenant admin client")
		tr := g.Spec.TenantRef.SetDefaultNamespace(g.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("get minio group")
		_, err = mtac.GetGroupDescription(ctx, g.Spec.Name)
		exists := err == nil
		if exists && !g.Spec.Migrate {
			err = fmt.Errorf("group %s already exists", g.Spec.Name)
		}
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchGroup")
		if err != nil {
			return failure(err)
		}

		l.Info("create minio group")
		err = mtac.UpdateGroupMembers(ctx, madmin.GroupAddRemove{
			Group:    g.Spec.Name,
			IsRemove: false,
		})
		if err != nil {
			return failure(err)
		}

		l.Info("set status")
		g.Spec.Migrate = false
		g.Status.CurrentSpec = &g.Spec
		err = r.Update(ctx, g)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	return success()
}

<<<<<<< HEAD
<<<<<<< HEAD
=======
>>>>>>> 9f9811e (Add license headers)
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
<<<<<<< HEAD
=======
>>>>>>> 38bcac6 (Split internal/operator/operator.go into multiple files)
=======
>>>>>>> 9f9811e (Add license headers)
package operator

import (
	"context"
	"fmt"
	"reflect"
	"slices"
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

// minioGroupBindingReconciler reconciles [v1.MinioGroupBinding] resources
type minioGroupBindingReconciler struct {
	client.Client
	logger                 logr.Logger
	minioOperatorNamespace string
	syncInterval           time.Duration
}

// Builds a controller with a [minioGroupBindingReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioGroupBindingReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioGroupBinding{}).Build(r)
	if err != nil {
		return err
	}
	r.logger = ctrl.GetLogger()
	return nil
}

// +kubebuilder:rbac:groups=bfiola.dev,resources=miniogroupbindings,verbs=get;list;update;watch

// Reconciles a [reconcile.Request] associated with a [v1.MinioGroupBinding].
// Returns a error if reconciliation fails.
func (r *minioGroupBindingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	gb := &v1.MinioGroupBinding{}
	err := r.Get(ctx, req.NamespacedName, gb)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	success := func() (reconcile.Result, error) { return reconcile.Result{RequeueAfter: r.syncInterval}, nil }
	failure := func(err error) (reconcile.Result, error) { return reconcile.Result{}, err }
	deleteGroupMember := func() error {
		l.Info("get tenant admin client")
		tr := gb.Status.CurrentSpec.TenantRef.SetDefaultNamespace(gb.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return err
		}

		l.Info("delete minio group member")
		err = mtac.UpdateGroupMembers(ctx, madmin.GroupAddRemove{
			Group:    gb.Status.CurrentSpec.Group,
			Members:  []string{gb.Status.CurrentSpec.User},
			IsRemove: true,
		})
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchUser")
		if err != nil {
			return err
		}

		l.Info("clear status")
		gb.Status.CurrentSpec = nil
		err = r.Update(ctx, gb)
		if err != nil {
			return err
		}

		return nil
	}

	if !gb.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if gb.Status.CurrentSpec != nil {
			l.Info("delete group member (status set)")

			err = deleteGroupMember()
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(gb, finalizer)
		err = r.Update(ctx, gb)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if !controllerutil.ContainsFinalizer(gb, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(gb, finalizer)
		err = r.Update(ctx, gb)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if gb.Status.CurrentSpec != nil {
		l.Info("check for group binding change")

		if gb.Spec.Migrate {
			l.Info("remove migrate spec field")
			gb.Spec.Migrate = false
			err = r.Update(ctx, gb)
			if err != nil {
				return failure(err)
			}
		}

		l.Info("get tenant admin client")
		tr := gb.Status.CurrentSpec.TenantRef.SetDefaultNamespace(gb.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("get minio group")
		gd, err := mtac.GetGroupDescription(ctx, gb.Status.CurrentSpec.Group)
		if err != nil {
			return failure(err)
		}

		if !slices.Contains(gd.Members, gb.Status.CurrentSpec.User) {
			l.Info("clear status (group binding no longer exists)")
			gb.Status.CurrentSpec = nil
			err = r.Update(ctx, gb)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		if !reflect.DeepEqual(*gb.Status.CurrentSpec, gb.Spec) {
			l.Info("delete group member (status and spec differ)")
			err = deleteGroupMember()
			if err != nil {
				return failure(err)
			}

			return success()
		}
	}

	if gb.Status.CurrentSpec == nil {
		l.Info("add group member (status unset)")

		l.Info("get tenant admin client")
		tr := gb.Spec.TenantRef.SetDefaultNamespace(gb.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("get minio group description")
		gd, err := mtac.GetGroupDescription(ctx, gb.Spec.Group)
		if err != nil {
			return failure(err)
		}
		exists := slices.Contains(gd.Members, gb.Spec.User)
		if exists && !gb.Spec.Migrate {
			err = fmt.Errorf("user %s already member of group %s", gb.Spec.User, gb.Spec.Group)
		}
		if err != nil {
			return failure(err)
		}

		l.Info("add minio group member")
		err = mtac.UpdateGroupMembers(ctx, madmin.GroupAddRemove{
			Group:    gb.Spec.Group,
			Members:  []string{gb.Spec.User},
			IsRemove: false,
		})
		if err != nil {
			return failure(err)
		}

		l.Info("set status")
		gb.Spec.Migrate = false
		gb.Status.CurrentSpec = &gb.Spec
		err = r.Update(ctx, gb)
		if err != nil {
			return failure(err)
		}

		return success()

	}

	return success()
}

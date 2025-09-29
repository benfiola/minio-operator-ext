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
	"reflect"
	"slices"
	"strings"
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

// minioPolicyBindingReconciler reconciles [v1.MinioPolicyBinding] resources
type minioPolicyBindingReconciler struct {
	client.Client
	logger                 logr.Logger
	minioOperatorNamespace string
	syncInterval           time.Duration
}

// Builds a controller with a [minioPolicyBindingReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioPolicyBindingReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioPolicyBinding{}).Build(r)
	if err != nil {
		return err
	}
	r.logger = ctrl.GetLogger()
	return nil
}

// +kubebuilder:rbac:groups=bfiola.dev,resources=miniopolicybindings,verbs=get;list;update;watch

// Reconciles a [reconcile.Request] associated with a [v1.MinioPolicyBinding].
// Returns a error if reconciliation fails.
func (r *minioPolicyBindingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	pb := &v1.MinioPolicyBinding{}
	err := r.Get(ctx, req.NamespacedName, pb)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	success := func() (reconcile.Result, error) { return reconcile.Result{RequeueAfter: r.syncInterval}, nil }
	failure := func(err error) (reconcile.Result, error) { return reconcile.Result{}, err }
	detachPolicyMember := func() error {
		l.Info("get tenant admin client")
		tr := pb.Status.CurrentSpec.TenantRef.SetDefaultNamespace(pb.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return err
		}

		l.Info("detach minio policy from identity")
		ldap := pb.Status.CurrentSpec.Group.Ldap != "" || pb.Status.CurrentSpec.User.Ldap != ""
		if ldap {
			_, err = mtac.DetachPolicyLDAP(ctx, madmin.PolicyAssociationReq{
				Group:    pb.Status.CurrentSpec.Group.Ldap,
				Policies: []string{pb.Status.CurrentSpec.Policy},
				User:     pb.Status.CurrentSpec.User.Ldap,
			})
		} else {
			_, err = mtac.DetachPolicy(ctx, madmin.PolicyAssociationReq{
				Group:    pb.Status.CurrentSpec.Group.Builtin,
				Policies: []string{pb.Status.CurrentSpec.Policy},
				User:     pb.Status.CurrentSpec.User.Builtin,
			})
		}
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchUser")
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchGroup")
		if err != nil {
			return err
		}

		l.Info("clear status")
		pb.Status.CurrentSpec = nil
		err = r.Update(ctx, pb)
		if err != nil {
			return err
		}

		return nil
	}

	if !pb.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if pb.Status.CurrentSpec != nil {
			l.Info("detach policy member (status set)")

			err = detachPolicyMember()
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(pb, finalizer)
		err = r.Update(ctx, pb)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if !controllerutil.ContainsFinalizer(pb, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(pb, finalizer)
		err = r.Update(ctx, pb)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if pb.Status.CurrentSpec != nil {
		l.Info("check for policy binding change")

		if pb.Spec.Migrate {
			l.Info("remove migrate spec field")
			pb.Spec.Migrate = false
			err = r.Update(ctx, pb)
			if err != nil {
				return failure(err)
			}
		}

		l.Info("get tenant admin client")
		tr := pb.Status.CurrentSpec.TenantRef.SetDefaultNamespace(pb.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		if !reflect.DeepEqual(*pb.Status.CurrentSpec, pb.Spec) {
			l.Info("detach policy member (status and spec differ)")
			err = detachPolicyMember()
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("list policy entities")
		ldap := pb.Status.CurrentSpec.Group.Ldap != "" || pb.Status.CurrentSpec.User.Ldap != ""
		found := false
		if ldap {
			pes, err := mac.GetLDAPPolicyEntities(ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Status.CurrentSpec.Policy}})
			if err != nil {
				return failure(err)
			}
			ipdc, err := mac.GetIDPConfig(ctx, "ldap", "")
			if err != nil {
				return failure(err)
			}
			usbdn := ""
			for _, ipdci := range ipdc.Info {
				if ipdci.Key != "user_dn_search_base_dn" {
					continue
				}
				usbdn = ipdci.Value
				break
			}
			for _, pm := range pes.PolicyMappings {
				u := pb.Status.CurrentSpec.User.Ldap
				if u != "" && usbdn != "" && !strings.HasPrefix(u, "cn=") {
					u = fmt.Sprintf("cn=%s,%s", u, usbdn)
				}
				if slices.Contains(pm.Groups, pb.Status.CurrentSpec.Group.Ldap) || slices.Contains(pm.Users, u) {
					found = true
					break
				}
			}
		} else {
			pes, err := mac.GetPolicyEntities(ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Status.CurrentSpec.Policy}})
			if err != nil {
				return failure(err)
			}
			for _, pm := range pes.PolicyMappings {
				if slices.Contains(pm.Groups, pb.Status.CurrentSpec.Group.Builtin) || slices.Contains(pm.Users, pb.Status.CurrentSpec.User.Builtin) {
					found = true
					break
				}
			}
		}
		if !found {
			l.Info("clear status (policy binding no longer found)")
			pb.Status.CurrentSpec = nil
			err = r.Update(ctx, pb)
			if err != nil {
				return failure(err)
			}

			return success()
		}
	}

	if pb.Status.CurrentSpec == nil {
		l.Info("attach policy member (status unset)")

		l.Info("get tenant admin client")
		tr := pb.Spec.TenantRef.SetDefaultNamespace(pb.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("attach minio policy to identity")
		ldap := pb.Spec.Group.Ldap != "" || pb.Spec.User.Ldap != ""
		if ldap {
			_, err = mtac.AttachPolicyLDAP(ctx, madmin.PolicyAssociationReq{
				Group:    pb.Spec.Group.Ldap,
				Policies: []string{pb.Spec.Policy},
				User:     pb.Spec.User.Ldap,
			})
		} else {
			_, err = mtac.AttachPolicy(ctx, madmin.PolicyAssociationReq{
				Group:    pb.Spec.Group.Builtin,
				Policies: []string{pb.Spec.Policy},
				User:     pb.Spec.User.Builtin,
			})
		}
		if pb.Spec.Migrate {
			err = ignoreMadminErrorCode(err, "XMinioAdminPolicyChangeAlreadyApplied")
		}
		if err != nil {
			return failure(err)
		}

		l.Info("set status")
		pb.Spec.Migrate = false
		pb.Status.CurrentSpec = &pb.Spec
		err = r.Update(ctx, pb)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	return success()
}

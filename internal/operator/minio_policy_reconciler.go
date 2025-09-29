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
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	v1 "github.com/benfiola/minio-operator-ext/pkg/api/bfiola.dev/v1"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// minioPolicyReconciler reconciles [v1.MinioPolicy] resources
type minioPolicyReconciler struct {
	client.Client
	logger                 logr.Logger
	minioOperatorNamespace string
	syncInterval           time.Duration
}

// Builds a controller with a [minioPolicyReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioPolicyReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioPolicy{}).Build(r)
	if err != nil {
		return err
	}
	r.logger = ctrl.GetLogger()
	return nil
}

// Used to compute whether a minio policy has changed
func getHash(o interface{}) string {
	hw := sha512.New512_256()
	mps, ok := o.(*v1.MinioPolicySpec)
	if ok {
		var d []string
		var a []string
		d = append(d, mps.Version)
		for _, ps := range mps.Statement {
			a = append(a, getHash(ps))
		}
		slices.Sort(a)
		d = append(d, strings.Join(a, ","))
		hw.Write([]byte(strings.Join(d, "|")))
	}
	mpst, ok := o.(v1.MinioPolicyStatement)
	if ok {
		var d []string
		var a []string
		a = append(a, mpst.Action...)
		slices.Sort(a)
		d = append(d, strings.Join(a, ","))
		d = append(d, mpst.Effect)
		a = []string{}
		a = append(a, mpst.Resource...)
		slices.Sort(a)
		d = append(d, strings.Join(a, ","))
		hw.Write([]byte(strings.Join(d, "|")))
	}
	h := hw.Sum(nil)
	return string(h)
}

// +kubebuilder:rbac:groups=bfiola.dev,resources=miniopolicies,verbs=get;list;update;watch

// Reconciles a [reconcile.Request] associated with a [v1.MinioPolicy].
// Returns a error if reconciliation fails.
func (r *minioPolicyReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	p := &v1.MinioPolicy{}
	err := r.Get(ctx, req.NamespacedName, p)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	success := func() (reconcile.Result, error) { return reconcile.Result{RequeueAfter: r.syncInterval}, nil }
	failure := func(err error) (reconcile.Result, error) { return reconcile.Result{}, err }

	if !p.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if p.Status.CurrentSpec != nil {
			l.Info("delete policy (status set)")

			l.Info("get tenant client")
			tr := p.Status.CurrentSpec.TenantRef.SetDefaultNamespace(p.GetNamespace())
			mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
			if err != nil {
				return failure(err)
			}
			mtac, err := mtci.GetAdminClient(ctx)
			if err != nil {
				return failure(err)
			}

			l.Info("delete minio policy")
			err = mtac.RemoveCannedPolicy(ctx, p.Status.CurrentSpec.Name)
			if err != nil {
				return failure(err)
			}

			l.Info("clear status")
			p.Status.CurrentSpec = nil
			err = r.Update(ctx, p)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(p, finalizer)
		err = r.Update(ctx, p)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if !controllerutil.ContainsFinalizer(p, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(p, finalizer)
		err = r.Update(ctx, p)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if p.Status.CurrentSpec != nil {
		l.Info("check for policy change")

		if p.Spec.Migrate {
			l.Info("remove migrate spec field")
			p.Spec.Migrate = false
			err = r.Update(ctx, p)
			if err != nil {
				return failure(err)
			}
		}

		l.Info("get tenant admin client")
		tr := p.Status.CurrentSpec.TenantRef.SetDefaultNamespace(p.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("get minio policy")
		mp, err := mtac.InfoCannedPolicyV2(ctx, p.Status.CurrentSpec.Name)
		e := !isMadminErrorCode(err, "XMinioAdminNoSuchPolicy")
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchPolicy")
		if err != nil {
			return failure(err)
		}
		if !e {
			l.Info("clear status (minio policy no longer exists)")
			p.Status.CurrentSpec = nil
			err = r.Update(ctx, p)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		if !reflect.DeepEqual(p.Status.CurrentSpec.Version, p.Spec.Version) || !reflect.DeepEqual(p.Status.CurrentSpec.Statement, p.Spec.Statement) {
			l.Info("update policy (statement or version changed)")

			l.Info("marshal policy")
			pd, err := json.Marshal(map[string]any{
				"statement": p.Spec.Statement,
				"version":   p.Spec.Version,
			})
			if err != nil {
				return failure(err)
			}

			l.Info("update minio policy")
			err = mtac.AddCannedPolicy(ctx, p.Status.CurrentSpec.Name, pd)
			if err != nil {
				return failure(err)
			}

			l.Info("set status")
			p.Status.CurrentSpec.Statement = p.Spec.Statement
			p.Status.CurrentSpec.Version = p.Spec.Version
			err = r.Update(ctx, p)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("unmarshal minio policy")
		mps := &v1.MinioPolicySpec{}
		err = json.Unmarshal(mp.Policy, mps)
		if err != nil {
			return failure(err)
		}

		if getHash(mps) != getHash(p.Status.CurrentSpec) {
			l.Info("update policy (minio policy and spec differ)")

			l.Info("marshal policy")
			pd, err := json.Marshal(map[string]any{
				"statement": p.Status.CurrentSpec.Statement,
				"version":   p.Status.CurrentSpec.Version,
			})
			if err != nil {
				return failure(err)
			}

			l.Info("update minio policy")
			err = mtac.AddCannedPolicy(ctx, p.Status.CurrentSpec.Name, pd)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		return success()
	}

	if p.Status.CurrentSpec == nil {
		l.Info("create policy (status unset)")

		l.Info("get tenant admin client")
		tr := p.Spec.TenantRef.SetDefaultNamespace(p.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr, minioTenantClientInfoOpts{minioOperatorNamespace: r.minioOperatorNamespace})
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("get minio policy")
		_, err = mtac.InfoCannedPolicyV2(ctx, p.Spec.Name)
		exists := err == nil
		if exists && !p.Spec.Migrate {
			err = fmt.Errorf("policy %s already exists", p.Spec.Name)
		}
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchPolicy")
		if exists && !p.Spec.Migrate {
			return failure(err)
		}

		l.Info("marshal policy to json")
		pd, err := json.Marshal(map[string]any{
			"statement": p.Spec.Statement,
			"version":   p.Spec.Version,
		})
		if err != nil {
			return failure(err)
		}

		l.Info("create minio policy")
		err = mtac.AddCannedPolicy(ctx, p.Spec.Name, pd)
		if err != nil {
			return failure(err)
		}

		l.Info("set status")
		p.Spec.Migrate = false
		p.Status.CurrentSpec = &p.Spec
		err = r.Update(ctx, p)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	return success()
}

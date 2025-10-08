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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// minioAccessKeyReconciler reconciles [v1.MinioAccessKey] resources
type minioAccessKeyReconciler struct {
	client.Client
	logger                 logr.Logger
	minioOperatorNamespace string
	syncInterval           time.Duration
}

// Builds a controller with a [minioAccessKeyReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioAccessKeyReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioAccessKey{}).Build(r)
	if err != nil {
		return err
	}
	r.logger = ctrl.GetLogger()
	return nil
}

// +kubebuilder:rbac:groups=bfiola.dev,resources=minioaccesskeys,verbs=get;list;update;watch

// Reconciles a [reconcile.Request] associated with a [v1.MinioAccessKey].
// Returns a error if reconciliation fails.
func (r *minioAccessKeyReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	ak := &v1.MinioAccessKey{}
	err := r.Get(ctx, req.NamespacedName, ak)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !ak.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")
		err = r.deleteAccessKey(ctx, l, ak)
		if err != nil {
			return r.fail(err)
		}

		return r.succeed()
	}

	if !controllerutil.ContainsFinalizer(ak, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(ak, finalizer)
		err = r.Update(ctx, ak)
		if err != nil {
			return r.fail(err)
		}

		return r.succeed()
	}

	if ak.Status.CurrentSpec != nil {
		l.Info("check for access key change")

		err := r.updateAccessKey(ctx, ak, l)
		if err != nil {
			return r.fail(err)
		}
		return r.succeed()
	}

	if ak.Status.CurrentSpec == nil {
		l.Info("create access key (status unset)")

		creds, err := r.createAccessKey(ctx, l, ak)
		if err != nil {
			return r.fail(err)
		}

		err = r.createCredentialsSecret(ctx, l, ak, creds)
		if err != nil {
			return r.fail(err)
		}

		return r.succeed()
	}

	return r.succeed()
}

func (r *minioAccessKeyReconciler) updateAccessKey(ctx context.Context, sa *v1.MinioAccessKey, l logr.Logger) error {
	if sa.Spec.Migrate {
		l.Info("remove migrate spec field")
		sa.Spec.Migrate = false
		err := r.Update(ctx, sa)
		if err != nil {
			return err
		}
	}

	if sa.Status.CurrentSpec.AccessKey == "" {
		return fmt.Errorf("access key is required. Resource: name=%s namespace=%s", sa.GetName(), sa.GetNamespace())
	}

	l.Info("get tenant admin client")
	mtac, err := r.getAdminClient(ctx, sa.Status.CurrentSpec.TenantRef.Name, sa.GetNamespace())
	if err != nil {
		return err
	}

	l.Info("get minio access key")
	_, err = mtac.InfoServiceAccount(ctx, sa.Status.CurrentSpec.AccessKey)
	e := !isMadminErrorCode(err, "XMinioInvalidIAMCredentials")
	err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
	if err != nil {
		return err
	}
	if !e {
		l.Info("clear status (minio access key no longer exists)")
		sa.Status.CurrentSpec = nil
		err = r.Update(ctx, sa)
		if err != nil {
			return err
		}

		return err
	}

	// TODO: update access key
	// mtac.UpdateServiceAccount(ctx, sa.Status.CurrentSpec.AccessKey, madmin.UpdateServiceAccountReq{})

	return nil
}

func (r *minioAccessKeyReconciler) createAccessKey(ctx context.Context, l logr.Logger, sa *v1.MinioAccessKey) (*madmin.Credentials, error) {
	l.Info("get tenant admin client")
	mtac, err := r.getAdminClient(ctx, sa.Spec.TenantRef.Name, sa.GetNamespace())
	if err != nil {
		return nil, err
	}

	if sa.Spec.AccessKey != "" {
		if sa.Spec.Migrate {
			return nil, fmt.Errorf("cannot migrate access key without access key. Resource: name=%s namespace=%s", sa.GetName(), sa.GetNamespace())
		}

		l.Info("get minio access key")
		_, err = mtac.InfoServiceAccount(ctx, sa.Spec.AccessKey)
		exists := err == nil
		if exists && !sa.Spec.Migrate {
			err = fmt.Errorf("access key %s already exists", sa.Spec.AccessKey)
		}
		err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
		if err != nil {
			return nil, err
		}
	}

	var expiration *time.Time
	if sa.Spec.Expiration != nil {
		t := sa.Spec.Expiration.Time
		expiration = &t
	}

	secretKey := ""
	if sa.Spec.SecretKeyRef != (v1.ResourceKeyRef{}) {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: sa.Spec.SecretKeyRef.Name, Namespace: sa.Spec.SecretKeyRef.Namespace}, secret)
		if err != nil {
			return nil, err
		}
		secretKey = string(secret.Data[sa.Spec.SecretKeyRef.Key])
	}

	ldap := false
	user := sa.Spec.User.Builtin
	if sa.Spec.User.Ldap != "" {
		user = sa.Spec.User.Ldap
		ldap = true
	}

	req := madmin.AddServiceAccountReq{
		AccessKey:   sa.Spec.AccessKey,
		Description: sa.Spec.Description,
		Expiration:  expiration,
		Name:        sa.Spec.Name,
		SecretKey:   secretKey,
		TargetUser:  user,
		// TODO: implement
		// Policy:      nil,
	}

	l.Info("create minio access key")
	var creds madmin.Credentials
	if ldap {
		creds, err = mtac.AddServiceAccountLDAP(ctx, req)
	} else {
		creds, err = mtac.AddServiceAccount(ctx, req)
	}
	if err != nil {
		return nil, err
	}

	l.Info("set status")
	sa.Spec.Migrate = false
	sa.Status.CurrentSpec = &sa.Spec
	sa.Status.CurrentSpec.AccessKey = creds.AccessKey
	err = r.Update(ctx, sa)
	if err != nil {
		return nil, err
	}

	return &creds, nil
}

func (r *minioAccessKeyReconciler) deleteAccessKey(ctx context.Context, l logr.Logger, sa *v1.MinioAccessKey) error {
	if sa.Status.CurrentSpec != nil {
		l.Info("delete access key (status set)")

		if sa.Status.CurrentSpec.AccessKey == "" {
			return fmt.Errorf("access key is required. Resource: name=%s namespace=%s", sa.GetName(), sa.GetNamespace())
		}

		l.Info("get tenant admin client")
		mtac, err := r.getAdminClient(ctx, sa.Status.CurrentSpec.TenantRef.Name, sa.GetNamespace())
		if err != nil {
			return err
		}

		l.Info("delete minio access key")
		err = mtac.DeleteServiceAccount(ctx, sa.Status.CurrentSpec.AccessKey)
		err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
		if err != nil {
			return err
		}

		l.Info("clear status")
		sa.Status.CurrentSpec = nil
		err = r.Update(ctx, sa)
		if err != nil {
			return err
		}

		return nil
	}

	l.Info("clear finalizer")
	controllerutil.RemoveFinalizer(sa, finalizer)
	err := r.Update(ctx, sa)
	if err != nil {
		return err
	}

	return nil
}

func (r *minioAccessKeyReconciler) getAdminClient(ctx context.Context, name, namespace string) (*madmin.AdminClient, error) {
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

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=create

func (r *minioAccessKeyReconciler) createCredentialsSecret(ctx context.Context, l logr.Logger, sa *v1.MinioAccessKey, creds *madmin.Credentials) error {
	l.Info("create or update credentials secret")

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sa.GetNamespace(),
			Name:      sa.Spec.SecretName,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"accessKey": []byte(creds.AccessKey),
			"secretKey": []byte(creds.SecretKey),
		},
	}

	err := controllerutil.SetOwnerReference(sa, desired, r.Scheme())
	if err != nil {
		return err
	}

	return r.Create(ctx, desired)
}

func (r *minioAccessKeyReconciler) succeed() (reconcile.Result, error) {
	return reconcile.Result{RequeueAfter: r.syncInterval}, nil
}

func (r *minioAccessKeyReconciler) fail(err error) (reconcile.Result, error) {
	return reconcile.Result{}, err
}

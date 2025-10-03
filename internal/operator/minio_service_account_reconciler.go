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
		err = r.deleteServiceAccount(ctx, l, sa)
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

		err := r.updateServiceAccount(ctx, sa, l)
		if err != nil {
			return failedReconcilliation(err)
		}
		return successfulReconcilliation(r.syncInterval)
	}

	if sa.Status.CurrentSpec == nil {
		l.Info("create service account (status unset)")

		creds, err := r.createServiceAccount(ctx, l, sa)
		if err != nil {
			return failedReconcilliation(err)
		}

		err = r.createCredentialsSecret(ctx, l, sa, creds)
		if err != nil {
			return failedReconcilliation(err)
		}

		return successfulReconcilliation(r.syncInterval)
	}

	return successfulReconcilliation(r.syncInterval)
}

func (r *minioServiceAccountReconciler) updateServiceAccount(ctx context.Context, sa *v1.MinioServiceAccount, l logr.Logger) error {
	if sa.Spec.Migrate {
		l.Info("remove migrate spec field")
		sa.Spec.Migrate = false
		err := r.Update(ctx, sa)
		if err != nil {
			return err
		}
	}

	if sa.Status.CurrentSpec.AccessKey == nil {
		return fmt.Errorf("access key is required. Resource: name=%s namespace=%s", sa.GetName(), sa.GetNamespace())
	}

	l.Info("get tenant admin client")
	mtac, err := r.getAdminClient(ctx, sa.Status.CurrentSpec.TenantRef.Name, sa.GetNamespace())
	if err != nil {
		return err
	}

	l.Info("get minio service account")
	_, err = mtac.InfoServiceAccount(ctx, *sa.Status.CurrentSpec.AccessKey)
	e := !isMadminErrorCode(err, "XMinioInvalidIAMCredentials")
	err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
	if err != nil {
		return err
	}
	if !e {
		l.Info("clear status (minio service account no longer exists)")
		sa.Status.CurrentSpec = nil
		err = r.Update(ctx, sa)
		if err != nil {
			return err
		}

		return err
	}

	// TODO: update service account
	// mtac.UpdateServiceAccount(ctx, sa.Status.CurrentSpec.AccessKey, madmin.UpdateServiceAccountReq{})

	return nil
}

func (r *minioServiceAccountReconciler) createServiceAccount(ctx context.Context, l logr.Logger, sa *v1.MinioServiceAccount) (*madmin.Credentials, error) {
	l.Info("get tenant admin client")
	mtac, err := r.getAdminClient(ctx, sa.Spec.TenantRef.Name, sa.GetNamespace())
	if err != nil {
		return nil, err
	}

	if sa.Spec.AccessKey != nil {
		if sa.Spec.Migrate {
			return nil, fmt.Errorf("cannot migrate service account without access key. Resource: name=%s namespace=%s", sa.GetName(), sa.GetNamespace())
		}

		l.Info("get minio service account")
		_, err = mtac.InfoServiceAccount(ctx, *sa.Spec.AccessKey)
		exists := err == nil
		if exists && !sa.Spec.Migrate {
			err = fmt.Errorf("service account %s already exists", *sa.Spec.AccessKey)
		}
		err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
		if err != nil {
			return nil, err
		}
	}

	req := madmin.AddServiceAccountReq{
		TargetUser: sa.Spec.TargetUser,
		// TODO: implement
		// Policy:      nil,
		// Expiration:  sa.Spec.Expiration,
	}

	if sa.Spec.Name != nil {
		req.Name = *sa.Spec.Name
	}
	if sa.Spec.Description != nil {
		req.Description = *sa.Spec.Description
	}

	if sa.Spec.AccessKey != nil {
		req.AccessKey = *sa.Spec.AccessKey
	}

	// SecretKeyRef is specified, use the secret key
	if sa.Spec.SecretKeyRef != nil {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: sa.Spec.SecretKeyRef.Name, Namespace: sa.Spec.SecretKeyRef.Namespace}, secret)
		if err != nil {
			return nil, err
		}
		req.SecretKey = string(secret.Data[sa.Spec.SecretKeyRef.Key])
	}

	l.Info("create minio service account")
	creds, err := mtac.AddServiceAccount(ctx, req)
	if err != nil {
		return nil, err
	}

	l.Info("set status")
	sa.Spec.Migrate = false
	sa.Status.CurrentSpec = &sa.Spec
	if sa.Spec.AccessKey != nil {
		sa.Status.CurrentSpec.AccessKey = &creds.AccessKey
	}
	err = r.Update(ctx, sa)
	if err != nil {
		return nil, err
	}

	return &creds, nil
}

func (r *minioServiceAccountReconciler) deleteServiceAccount(ctx context.Context, l logr.Logger, sa *v1.MinioServiceAccount) error {
	if sa.Status.CurrentSpec != nil {
		l.Info("delete service account (status set)")

		if sa.Status.CurrentSpec.AccessKey == nil {
			return fmt.Errorf("access key is required. Resource: name=%s namespace=%s", sa.GetName(), sa.GetNamespace())
		}

		l.Info("get tenant admin client")
		mtac, err := r.getAdminClient(ctx, sa.Status.CurrentSpec.TenantRef.Name, sa.GetNamespace())
		if err != nil {
			return err
		}

		l.Info("delete minio service account")
		err = mtac.DeleteServiceAccount(ctx, *sa.Status.CurrentSpec.AccessKey)
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

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=create

func (r *minioServiceAccountReconciler) createCredentialsSecret(ctx context.Context, l logr.Logger, sa *v1.MinioServiceAccount, creds *madmin.Credentials) error {
	l.Info("create or update credentials secret")

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: sa.GetNamespace(),
			Name:      sa.Spec.TargetSecretName,
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

func successfulReconcilliation(duration time.Duration) (reconcile.Result, error) {
	return reconcile.Result{RequeueAfter: duration}, nil
}

func failedReconcilliation(err error) (reconcile.Result, error) {
	return reconcile.Result{}, err
}

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
	"fmt"
	"reflect"
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

		l.Info("clear status")
		ak.Status.CurrentSpec = nil
		err := r.Update(ctx, ak)
		if err != nil {
			return r.fail(err)
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(ak, finalizer)
		err = r.Update(ctx, ak)
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

		l.Info("set status")
		ak.Status.CurrentSpec = &ak.Spec
		err = r.Update(ctx, ak)
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

		l.Info("set status")
		ak.Spec.AccessKey = creds.AccessKey
		ak.Status.CurrentSpec = &ak.Spec
		err = r.Update(ctx, ak)
		if err != nil {
			return r.fail(err)
		}

		return r.succeed()
	}

	return r.succeed()
}

func (r *minioAccessKeyReconciler) updateAccessKey(ctx context.Context, ak *v1.MinioAccessKey, l logr.Logger) error {
	if ak.Spec.Migrate {
		l.Info("remove migrate spec field")
		ak.Spec.Migrate = false
		err := r.Update(ctx, ak)
		if err != nil {
			return err
		}
	}

	if ak.Status.CurrentSpec.AccessKey == "" {
		return fmt.Errorf("access key is required. Resource: name=%s namespace=%s", ak.GetName(), ak.GetNamespace())
	}

	l.Info("get tenant admin client")
	mtac, err := r.getAdminClient(ctx, ak.Status.CurrentSpec.TenantRef.Name, ak.GetNamespace())
	if err != nil {
		return err
	}

	l.Info("get minio access key")
	msa, err := mtac.InfoServiceAccount(ctx, ak.Status.CurrentSpec.AccessKey)
	e := !isMadminErrorCode(err, "XMinioInvalidIAMCredentials")
	err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
	if err != nil {
		return err
	}
	if !e {
		l.Info("clear status (minio access key no longer exists)")
		ak.Status.CurrentSpec = nil
		err = r.Update(ctx, ak)
		if err != nil {
			return err
		}

		return err
	}

	var cExpiration *time.Time
	if ak.Status.CurrentSpec.Expiration != nil {
		t := ak.Status.CurrentSpec.Expiration.Time
		cExpiration = &t
	}

	mPolicy := v1.MinioAccessKeyPolicy{}
	if !msa.ImpliedPolicy && msa.Policy != "" {
		mp, err := mtac.InfoCannedPolicyV2(ctx, msa.Policy)
		if err != nil {
			return err
		}
		err = json.Unmarshal(mp.Policy, &mPolicy)
		if err != nil {
			return err
		}
	}

	if msa.Description != ak.Status.CurrentSpec.Description || !reflect.DeepEqual(msa.Expiration, cExpiration) || msa.Name != ak.Status.CurrentSpec.Name || getHash(mPolicy) != getHash(ak.Status.CurrentSpec.Policy) {
		l.Info("update access key (status and remote differ)")
		err := mtac.UpdateServiceAccount(ctx, ak.Status.CurrentSpec.AccessKey, madmin.UpdateServiceAccountReq{
			NewDescription: ak.Status.CurrentSpec.Description,
			NewExpiration:  cExpiration,
			NewName:        ak.Status.CurrentSpec.Name,
		})
		if err != nil {
			return err
		}
	}

	if ak.Status.CurrentSpec.Description != ak.Spec.Description || !reflect.DeepEqual(ak.Status.CurrentSpec.Expiration, ak.Spec.Expiration) || ak.Status.CurrentSpec.Name != ak.Spec.Name || getHash(ak.Status.CurrentSpec.Policy) != getHash(ak.Spec.Policy) {
		l.Info("update access key (status and spec differ)")

		var expiration *time.Time
		if ak.Spec.Expiration != nil {
			t := ak.Spec.Expiration.Time
			expiration = &t
		}

		policy := []byte{}
		if getHash(ak.Spec.Policy) != getHash(v1.MinioAccessKeyPolicy{}) {
			policy, err = json.Marshal(map[string]any{
				"statement": ak.Spec.Policy.Statement,
				"version":   ak.Spec.Policy.Version,
			})
			if err != nil {
				return err
			}
		}

		err := mtac.UpdateServiceAccount(ctx, ak.Status.CurrentSpec.AccessKey, madmin.UpdateServiceAccountReq{
			NewDescription: ak.Spec.Description,
			NewExpiration:  expiration,
			NewPolicy:      policy,
			NewName:        ak.Spec.Name,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *minioAccessKeyReconciler) createAccessKey(ctx context.Context, l logr.Logger, ak *v1.MinioAccessKey) (*madmin.Credentials, error) {
	l.Info("get tenant admin client")
	mtac, err := r.getAdminClient(ctx, ak.Spec.TenantRef.Name, ak.GetNamespace())
	if err != nil {
		return nil, err
	}

	if ak.Spec.AccessKey != "" {
		if ak.Spec.Migrate {
			return nil, fmt.Errorf("cannot migrate access key without access key. Resource: name=%s namespace=%s", ak.GetName(), ak.GetNamespace())
		}

		l.Info("get minio access key")
		_, err = mtac.InfoServiceAccount(ctx, ak.Spec.AccessKey)
		exists := err == nil
		if exists && !ak.Spec.Migrate {
			err = fmt.Errorf("access key %s already exists", ak.Spec.AccessKey)
		}
		err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
		if err != nil {
			return nil, err
		}
	}

	var expiration *time.Time
	if ak.Spec.Expiration != nil {
		t := ak.Spec.Expiration.Time
		expiration = &t
	}

	var policy []byte
	if getHash(ak.Spec.Policy) != getHash(v1.MinioAccessKeyPolicy{}) {
		policy, err = json.Marshal(map[string]any{
			"statement": ak.Spec.Policy.Statement,
			"version":   ak.Spec.Policy.Version,
		})
		if err != nil {
			return nil, err
		}
	}

	secretKey := ""
	if ak.Spec.SecretKeyRef != (v1.ResourceKeyRef{}) {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: ak.Spec.SecretKeyRef.Name, Namespace: ak.Spec.SecretKeyRef.Namespace}, secret)
		if err != nil {
			return nil, err
		}
		bSecretKey, ok := secret.Data[ak.Spec.SecretKeyRef.Key]
		if !ok {
			return nil, fmt.Errorf("secretKeyRef key '%s' not found", ak.Spec.SecretKeyRef.Key)
		}
		secretKey = string(bSecretKey)
	}

	ldap := false
	user := ak.Spec.User.Builtin
	if ak.Spec.User.Ldap != "" {
		user = ak.Spec.User.Ldap
		ldap = true
	}

	req := madmin.AddServiceAccountReq{
		AccessKey:   ak.Spec.AccessKey,
		Description: ak.Spec.Description,
		Expiration:  expiration,
		Name:        ak.Spec.Name,
		SecretKey:   secretKey,
		Policy:      policy,
		TargetUser:  user,
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

	return &creds, nil
}

func (r *minioAccessKeyReconciler) deleteAccessKey(ctx context.Context, l logr.Logger, ak *v1.MinioAccessKey) error {
	if ak.Status.CurrentSpec != nil {
		l.Info("delete access key (status set)")

		if ak.Status.CurrentSpec.AccessKey == "" {
			return fmt.Errorf("access key is required. Resource: name=%s namespace=%s", ak.GetName(), ak.GetNamespace())
		}

		l.Info("get tenant admin client")
		mtac, err := r.getAdminClient(ctx, ak.Status.CurrentSpec.TenantRef.Name, ak.GetNamespace())
		if err != nil {
			return err
		}

		l.Info("delete minio access key")
		err = mtac.DeleteServiceAccount(ctx, ak.Status.CurrentSpec.AccessKey)
		err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
		if err != nil {
			return err
		}

		return nil
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

func (r *minioAccessKeyReconciler) createCredentialsSecret(ctx context.Context, l logr.Logger, ak *v1.MinioAccessKey, creds *madmin.Credentials) error {
	l.Info("create credentials secret")

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ak.GetNamespace(),
			Name:      ak.Spec.SecretName,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			accessKeyFieldAccessKey: []byte(creds.AccessKey),
			accessKeyFieldSecretKey: []byte(creds.SecretKey),
		},
	}

	err := controllerutil.SetOwnerReference(ak, desired, r.Scheme())
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

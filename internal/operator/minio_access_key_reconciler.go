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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"

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

// +kubebuilder:rbac:groups=bfiola.dev,resources=minioaccesskeys,verbs=list;watch;

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

// +kubebuilder:rbac:groups=bfiola.dev,resources=minioaccesskeys,verbs=get;update;

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

		secret, err := r.updateCredentialsSecret(ctx, l, ak)
		if err != nil {
			return r.fail(err)
		}

		err = r.updateAccessKey(ctx, l, ak, secret)
		if err != nil {
			return r.fail(err)
		}

		l.Info("set status")
		ak.Status.CurrentSpec = &ak.Spec
		ak.Status.CurrentSecretResourceVersion = secret.ResourceVersion
		err = r.Update(ctx, ak)
		if err != nil {
			return r.fail(err)
		}

		return r.succeed()
	}

	if ak.Status.CurrentSpec == nil {
		l.Info("create access key (status unset)")
		secret, err := r.createCredentialsSecret(ctx, l, ak)
		if err != nil {
			return r.fail(err)
		}

		err = r.createAccessKey(ctx, l, ak, secret)
		if err != nil {
			r.Delete(ctx, secret)
			return r.fail(err)
		}

		l.Info("set status")
		// ensure that attempts to regenerate the secret use the same access key
		accessKey := string(secret.Data["accessKey"])
		ak.Spec.AccessKey = accessKey
		ak.Spec.Migrate = false
		ak.Status.CurrentSpec = &ak.Spec
		ak.Status.CurrentSecretResourceVersion = secret.ResourceVersion
		err = r.Update(ctx, ak)
		if err != nil {
			return r.fail(err)
		}

		return r.succeed()
	}

	return r.succeed()
}

// +kubebuilder:rbac:groups=bfiola.dev,resources=minioaccesskeys,verbs=update;

func (r *minioAccessKeyReconciler) updateAccessKey(ctx context.Context, l logr.Logger, ak *v1.MinioAccessKey, secret *corev1.Secret) error {
	if ak.Spec.Migrate {
		l.Info("remove migrate spec field")
		ak.Spec.Migrate = false
		err := r.Update(ctx, ak)
		if err != nil {
			return err
		}
	}

	l.Info("get tenant admin client")
	mtac, err := r.getAdminClient(ctx, ak.Status.CurrentSpec.TenantRef.Name, ak.GetNamespace())
	if err != nil {
		return err
	}

	l.Info("get current credentials")
	creds := r.getCredentialsFromSecret(secret)

	l.Info("get minio access key")
	msa, err := mtac.InfoServiceAccount(ctx, creds.AccessKey)
	e := !isMadminErrorCode(err, "XMinioInvalidIAMCredentials")
	err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
	if err != nil {
		return err
	}
	if !e {
		l.Info("clear status (minio access key no longer exists)")

		l.Info("delete secret")
		secret := corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: ak.Status.CurrentSpec.SecretName, Namespace: ak.GetNamespace()}, &secret)
		exists := true
		if errors.IsNotFound(err) {
			exists = false
			err = nil
		}
		if err != nil {
			return err
		}
		if exists {
			err = r.Delete(ctx, &secret)
			if err != nil {
				return err
			}
		}

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
		err = json.Unmarshal([]byte(msa.Policy), &mPolicy)
		if err != nil {
			return err
		}
	}

	if msa.Description != ak.Status.CurrentSpec.Description || !reflect.DeepEqual(msa.Expiration, cExpiration) || msa.Name != ak.Status.CurrentSpec.Name || getHash(&mPolicy) != getHash(&ak.Status.CurrentSpec.Policy) {
		l.Info("update access key (status and remote differ)")

		policy, err := json.Marshal(map[string]any{
			"statement": ak.Status.CurrentSpec.Policy.Statement,
			"version":   ak.Status.CurrentSpec.Policy.Version,
		})
		if err != nil {
			return err
		}

		err = mtac.UpdateServiceAccount(ctx, creds.AccessKey, madmin.UpdateServiceAccountReq{
			NewDescription: ak.Status.CurrentSpec.Description,
			NewExpiration:  cExpiration,
			NewName:        ak.Status.CurrentSpec.Name,
			NewPolicy:      policy,
			NewSecretKey:   creds.SecretKey,
		})
		if err != nil {
			return err
		}
	}

	if ak.Status.CurrentSpec.Description != ak.Spec.Description || !reflect.DeepEqual(ak.Status.CurrentSpec.Expiration, ak.Spec.Expiration) || ak.Status.CurrentSpec.Name != ak.Spec.Name || getHash(&ak.Status.CurrentSpec.Policy) != getHash(&ak.Spec.Policy) || secret.ResourceVersion != ak.Status.CurrentSecretResourceVersion {
		l.Info("update access key (status and spec differ)")

		var expiration *time.Time
		if ak.Spec.Expiration != nil {
			t := ak.Spec.Expiration.Time
			expiration = &t
		}

		policy := []byte{}
		if getHash(&ak.Spec.Policy) != getHash(&v1.MinioAccessKeyPolicy{}) {
			policy, err = json.Marshal(map[string]any{
				"statement": ak.Spec.Policy.Statement,
				"version":   ak.Spec.Policy.Version,
			})
			if err != nil {
				return err
			}
		}

		err := mtac.UpdateServiceAccount(ctx, creds.AccessKey, madmin.UpdateServiceAccountReq{
			NewDescription: ak.Spec.Description,
			NewExpiration:  expiration,
			NewName:        ak.Spec.Name,
			NewPolicy:      policy,
			NewSecretKey:   creds.SecretKey,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// Creates a MinIO access key for the given resource.
// If migrating the access key, ...
func (r *minioAccessKeyReconciler) createAccessKey(ctx context.Context, l logr.Logger, ak *v1.MinioAccessKey, secret *corev1.Secret) error {
	l.Info("get tenant admin client")
	mtac, err := r.getAdminClient(ctx, ak.Spec.TenantRef.Name, ak.GetNamespace())
	if err != nil {
		return err
	}

	l.Info("get current credentials")
	creds := r.getCredentialsFromSecret(secret)

	l.Info("get minio access key")
	_, err = mtac.InfoServiceAccount(ctx, creds.AccessKey)
	exists := err == nil
	if exists && !ak.Spec.Migrate {
		err = fmt.Errorf("access key already exists")
	}
	err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
	if err != nil {
		return err
	}

	var expiration *time.Time
	if ak.Spec.Expiration != nil {
		t := ak.Spec.Expiration.Time
		expiration = &t
	}

	var policy []byte
	if getHash(&ak.Spec.Policy) != getHash(&v1.MinioAccessKeyPolicy{}) {
		policy, err = json.Marshal(map[string]any{
			"statement": ak.Spec.Policy.Statement,
			"version":   ak.Spec.Policy.Version,
		})
		if err != nil {
			return err
		}
	}

	ldap := false
	user := ak.Spec.User.Builtin
	if ak.Spec.User.Ldap != "" {
		user = ak.Spec.User.Ldap
		ldap = true
	}

	req := madmin.AddServiceAccountReq{
		AccessKey:   creds.AccessKey,
		Description: ak.Spec.Description,
		Expiration:  expiration,
		Name:        ak.Spec.Name,
		SecretKey:   creds.SecretKey,
		Policy:      policy,
		TargetUser:  user,
	}

	l.Info("create minio access key")
	if ldap {
		_, err = mtac.AddServiceAccountLDAP(ctx, req)
	} else {
		_, err = mtac.AddServiceAccount(ctx, req)
	}
	if err != nil {
		return err
	}

	return nil
}

// Deletes a MinIO access key
func (r *minioAccessKeyReconciler) deleteAccessKey(ctx context.Context, l logr.Logger, ak *v1.MinioAccessKey) error {
	if ak.Status.CurrentSpec != nil {
		l.Info("delete access key (status set)")

		l.Info("get generated secret")
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: ak.Status.CurrentSpec.SecretName, Namespace: ak.GetNamespace()}, secret)
		if errors.IsNotFound(err) {
			l.Info("generated secret not found")
			// unable to delete resource if secret is not found
			return nil
		}
		if err != nil {
			return err
		}

		l.Info("get tenant admin client")
		mtac, err := r.getAdminClient(ctx, ak.Status.CurrentSpec.TenantRef.Name, ak.GetNamespace())
		if err != nil {
			return err
		}

		l.Info("delete minio access key")
		creds := r.getCredentialsFromSecret(secret)
		err = mtac.DeleteServiceAccount(ctx, creds.AccessKey)
		err = ignoreMadminErrorCode(err, "XMinioInvalidIAMCredentials")
		if err != nil {
			return err
		}

		return nil
	}

	return nil
}

// Helper function get the MinIO admin client for a tenant.
// Returns an error if unable to fetch this admin client.
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

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=create;

func (r *minioAccessKeyReconciler) createCredentialsSecret(ctx context.Context, l logr.Logger, ak *v1.MinioAccessKey) (*corev1.Secret, error) {
	l.Info("create credentials secret")

	l.Info("get desired credentials")
	creds, err := r.getDesiredCredentials(ctx, ak, nil)
	if err != nil {
		return nil, err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ak.GetNamespace(),
			Name:      ak.Spec.SecretName,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			AccessKeyFieldAccessKey: []byte(creds.AccessKey),
			AccessKeyFieldSecretKey: []byte(creds.SecretKey),
		},
	}

	err = controllerutil.SetOwnerReference(ak, secret, r.Scheme())
	if err != nil {
		return nil, err
	}

	err = r.Create(ctx, secret)
	if err != nil {
		return nil, err
	}

	return secret, nil
}

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=delete;get;update;

func (r *minioAccessKeyReconciler) updateCredentialsSecret(ctx context.Context, l logr.Logger, ak *v1.MinioAccessKey) (*corev1.Secret, error) {
	l.Info("get generated secret")
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: ak.Status.CurrentSpec.SecretName, Namespace: ak.GetNamespace()}, secret)
	exists := true
	if errors.IsNotFound(err) {
		exists = false
		err = nil
	}
	if err != nil {
		return nil, err
	}

	if ak.Spec.SecretName != ak.Status.CurrentSpec.SecretName {
		l.Info("delete secret (secret name changed)")
		err := r.Delete(ctx, secret)
		if errors.IsNotFound(err) {
			err = nil
		}
		if err != nil {
			return nil, err
		}
		exists = false

		l.Info("update status")
		ak.Status.CurrentSpec.SecretName = ak.Spec.SecretName
		err = r.Update(ctx, ak)
		if err != nil {
			return nil, err
		}
	}

	if !exists {
		l.Info("regenerate secret (secret no longer exists)")
		secret, err := r.createCredentialsSecret(ctx, l, ak)
		if err != nil {
			return nil, err
		}

		return secret, nil
	}

	l.Info("get current credentials")
	current := r.getCredentialsFromSecret(secret)
	if current == nil {
		// current credentials should exist by this point
		err = fmt.Errorf("could not determine current credentials")
	}
	if err != nil {
		return nil, err
	}

	l.Info("get desired credentials")
	desired, err := r.getDesiredCredentials(ctx, ak, current)
	if err != nil {
		return nil, err
	}

	if current.getHash() != desired.getHash() {
		l.Info("update secret (credentials changed)")
		secret.StringData = map[string]string{
			AccessKeyFieldAccessKey: desired.AccessKey,
			AccessKeyFieldSecretKey: desired.SecretKey,
		}

		err = r.Update(ctx, secret)
		if err != nil {
			return nil, err
		}
	}

	return secret, nil
}

// minioAccessKeyCredentials represent an access key and secret key pair.
type minioAccessKeyCredentials struct {
	AccessKey string
	SecretKey string
}

// Computes a hash of credentials.
// This determines whether we need to update our generated secret *and* issue a new call to minio to update the access key.
func (c *minioAccessKeyCredentials) getHash() string {
	var buf bytes.Buffer

	binary.Write(&buf, binary.BigEndian, uint32(len(c.AccessKey)))
	buf.WriteString(c.AccessKey)

	binary.Write(&buf, binary.BigEndian, uint32(len(c.SecretKey)))
	buf.WriteString(c.SecretKey)

	checksum := sha256.Sum256(buf.Bytes())
	digest := hex.EncodeToString(checksum[:])
	return digest
}

var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// Generates a random string using the same encoding as MinIO access/secret keys to the given length.
func (r *minioAccessKeyReconciler) createRandomString(length int) (string, error) {
	numBytes := (length*5 + 7) / 8
	buf := make([]byte, numBytes)

	_, err := rand.Read(buf)
	if err != nil {
		return "", err
	}

	key := crockford.EncodeToString(buf)[:length]
	return key, nil
}

// Gets credentials from the provided secret
func (r *minioAccessKeyReconciler) getCredentialsFromSecret(secret *corev1.Secret) *minioAccessKeyCredentials {
	creds := minioAccessKeyCredentials{
		AccessKey: string(secret.Data[AccessKeyFieldAccessKey]),
		SecretKey: string(secret.Data[AccessKeyFieldSecretKey]),
	}
	return &creds
}

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;

// Given a MinioAccessKey resource, determines the credentials for the resource
// Determines whether credentials are explicitly defined in the resource spec.
// Then, checks any generated secret to see if generated credentials have already been established.
// Otherwise, generates new credentials.
func (r *minioAccessKeyReconciler) getDesiredCredentials(ctx context.Context, ak *v1.MinioAccessKey, current *minioAccessKeyCredentials) (*minioAccessKeyCredentials, error) {
	creds := minioAccessKeyCredentials{}

	// determine the desired access key
	// check if access key is defined in the spec
	creds.AccessKey = ak.Spec.AccessKey
	if creds.AccessKey == "" && current != nil {
		// access key is auto-generated - use access key from existing generated secret
		creds.AccessKey = current.AccessKey
	}
	if creds.AccessKey == "" && !ak.Spec.Migrate {
		// existing auto-generated access key not found, generate a new one
		accessKey, err := r.createRandomString(20)
		if err != nil {
			return nil, err
		}
		creds.AccessKey = accessKey
	}

	// determine the desired secret key
	// check if secret key is defined in the spec
	if ak.Spec.SecretKeyRef != (v1.ResourceKeyRef{}) {
		skr := ak.Spec.SecretKeyRef.SetDefaultNamespace(ak.GetNamespace())
		secret := corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: skr.Name, Namespace: skr.Namespace}, &secret)
		if err != nil {
			return nil, err
		}
		bSecretKey, ok := secret.Data[ak.Spec.SecretKeyRef.Key]
		if !ok {
			err = fmt.Errorf("secret key ref has no key %s", ak.Spec.SecretKeyRef.Key)
			return nil, err
		}
		creds.SecretKey = string(bSecretKey)
	}
	if creds.SecretKey == "" && current != nil {
		// secret key is auto-generated - use secret key from existing generated secret
		creds.SecretKey = current.SecretKey
	}
	if creds.SecretKey == "" && !ak.Spec.Migrate {
		// existing auto-generated secret key not found - generate a new one
		secretKey, err := r.createRandomString(40)
		if err != nil {
			return nil, err
		}
		creds.SecretKey = secretKey
	}

	if ak.Spec.Migrate && (creds.AccessKey == "" || creds.SecretKey == "") {
		err := fmt.Errorf("cannot determine desired credentials for migrated access key")
		return nil, err
	}

	return &creds, nil
}

// Helper method to call when a reconciliation loop succeeds
func (r *minioAccessKeyReconciler) succeed() (reconcile.Result, error) {
	return reconcile.Result{RequeueAfter: r.syncInterval}, nil
}

// Helper method to call when a reconciliation loop fails
func (r *minioAccessKeyReconciler) fail(err error) (reconcile.Result, error) {
	return reconcile.Result{}, err
}

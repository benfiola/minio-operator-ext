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
package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/benfiola/minio-operator-ext/internal/operator"
	v1 "github.com/benfiola/minio-operator-ext/pkg/api/bfiola.dev/v1"
	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
	miniov2 "github.com/minio/operator/pkg/apis/minio.min.io/v2"
	"github.com/neilotoole/slogt"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Adds a test object to a test objects list.
// This list is used to help during between-test cleanup.
// See: [Setup]
func CreateTestObject[T client.Object](v T) T {
	testObjects = append(testObjects, v)
	return v
}

// Defines kubernetes objects referenced during tests.
// Using a static set of tests ensures that cleanup is consistent between test runs.
// The tests themselves should create these objects as needed.
// [Setup] handles the cleanup of these resources.
// NOTE: Ensure matching minio objects are removed in [Setup].
var (
	testObjects = []client.Object{}
	bucket      = CreateTestObject(&v1.MinioBucket{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "bucket"},
		Spec:       v1.MinioBucketSpec{DeletionPolicy: v1.MinioBucketDeletionPolicyIfEmpty, Name: "bucket", TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	builtinGroup = CreateTestObject(&v1.MinioGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "builtin-group"},
		Spec:       v1.MinioGroupSpec{Name: "builtin-group", TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	builtinUserSecret = CreateTestObject(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "builtin-user-secret"},
		StringData: map[string]string{"SecretKey": "password"},
	})
	builtinUser = CreateTestObject(&v1.MinioUser{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "builtin-minio-user"},
		Spec:       v1.MinioUserSpec{AccessKey: "builtin-user", SecretKeyRef: v1.ResourceKeyRef{Name: builtinUserSecret.Name, Key: "SecretKey"}, TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	builtinUser2 = CreateTestObject(&v1.MinioUser{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "builtin-minio-user2"},
		Spec:       v1.MinioUserSpec{AccessKey: "builtin-user-2", SecretKeyRef: v1.ResourceKeyRef{Name: builtinUserSecret.Name, Key: "SecretKey"}, TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	builtinGroupToBuiltinUser = CreateTestObject(&v1.MinioGroupBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "builtin-group-to-builtin-user"},
		Spec:       v1.MinioGroupBindingSpec{Group: builtinGroup.Spec.Name, User: builtinUser.Spec.AccessKey, TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	policy = CreateTestObject(&v1.MinioPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy"},
		Spec:       v1.MinioPolicySpec{Statement: []v1.MinioPolicyStatement{{Action: []string{"s3:*"}, Effect: "Allow", Resource: []string{"arn:aws:s3:::*"}}}, Name: "policy", TenantRef: v1.ResourceRef{Name: "tenant"}, Version: "2012-10-17"},
	})
	policyToBuiltinGroup = CreateTestObject(&v1.MinioPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-to-builtin-group"},
		Spec:       v1.MinioPolicyBindingSpec{Group: v1.MinioPolicyBindingIdentity{Builtin: builtinGroup.Spec.Name}, Policy: policy.Spec.Name, TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	policyToBuiltinUser = CreateTestObject(&v1.MinioPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-to-builtin-user"},
		Spec:       v1.MinioPolicyBindingSpec{User: v1.MinioPolicyBindingIdentity{Builtin: builtinUser.Spec.AccessKey}, Policy: policy.Spec.Name, TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	policyToLdapGroup = CreateTestObject(&v1.MinioPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-to-ldap-group"},
		Spec:       v1.MinioPolicyBindingSpec{Group: v1.MinioPolicyBindingIdentity{Ldap: "cn=ldap-group,ou=users,dc=example,dc=org"}, Policy: policy.Spec.Name, TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	policyToLdapUser = CreateTestObject(&v1.MinioPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-to-ldap-user"},
		Spec:       v1.MinioPolicyBindingSpec{User: v1.MinioPolicyBindingIdentity{Ldap: "cn=ldap-user1,ou=users,dc=example,dc=org"}, Policy: policy.Spec.Name, TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	policyToLdapUserShort = CreateTestObject(&v1.MinioPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "policy-to-ldap-user-short"},
		Spec:       v1.MinioPolicyBindingSpec{User: v1.MinioPolicyBindingIdentity{Ldap: "ldap-user1"}, Policy: policy.Spec.Name, TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	accessKeySecret = CreateTestObject(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "access-key-secret"},
	})
	accessKeySecret2 = CreateTestObject(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "access-key-secret-2"},
	})
	builtinAccessKey = CreateTestObject(&v1.MinioAccessKey{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "builtin-minio-access-key"},
		Spec:       v1.MinioAccessKeySpec{Name: "builtin-minio-access-key", User: v1.MinioAccessKeyIdentity{Builtin: builtinUser.Spec.AccessKey}, SecretName: accessKeySecret.GetName(), TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
	ldapAccessKey = CreateTestObject(&v1.MinioAccessKey{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ldap-minio-service-account"},
		Spec:       v1.MinioAccessKeySpec{Name: "ldap-minio-access-key", User: v1.MinioAccessKeyIdentity{Ldap: "ldap-user1"}, SecretName: accessKeySecret.GetName(), TenantRef: v1.ResourceRef{Name: "tenant"}},
	})
)

// TestData holds data used during tests
// See [Setup].
type TestData struct {
	Ctx        context.Context
	Kube       client.Client
	KubeConfig string
	Minio      minio.Client
	Madmin     madmin.AdminClient
	Require    require.Assertions
	T          testing.TB
}

// Cleans up existing test objects and removes resources from the minio tenant.
// Returns a set of data used across most (all?) tests.
func Setup(t testing.TB) TestData {
	t.Helper()

	require := require.New(t)
	ctx := context.Background()

	// create kube client
	kc := os.Getenv("KUBECONFIG")
	rcfg, err := clientcmd.BuildConfigFromFlags("", kc)
	require.NoError(err, "build client config")
	s := runtime.NewScheme()
	err = kscheme.AddToScheme(s)
	require.NoError(err, "add kubernetes resources to scheme")
	err = miniov2.AddToScheme(s)
	require.NoError(err, "add minio resources to scheme")
	err = v1.AddToScheme(s)
	require.NoError(err, "add minio-operator-ext resources to scheme")
	k, err := client.New(rcfg, client.Options{Scheme: s, Cache: &client.CacheOptions{}})
	require.NoError(err, "build client")

	// delete resources if they exist
	for _, r := range testObjects {
		nr := r.DeepCopyObject().(client.Object)
		err := k.Get(ctx, client.ObjectKeyFromObject(nr), nr)
		if err != nil && apierrors.IsNotFound(err) {
			continue
		}
		require.NoError(err, "fetch existing testing k8s resource")
		nr.SetFinalizers([]string{})
		err = k.Update(ctx, nr)
		require.NoError(err, "remove finalizers from testing k8s resource")
		err = k.Delete(ctx, nr)
		if err != nil && apierrors.IsNotFound(err) {
			err = nil
		}
		require.NoError(err, "delete testing k8s resource")
	}

	// create minio clients
	ht := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	m, err := minio.New("minio.default.svc", &minio.Options{Creds: credentials.NewStaticV4("minio", "minio123", ""), Secure: true, Transport: ht})
	require.NoError(err, "create minio client")
	ma, err := madmin.New("minio.default.svc", "minio", "minio123", true)
	require.NoError(err, "create minio admin client")
	ma.SetCustomTransport(ht)

	// remove resources
	err = m.RemoveBucketWithOptions(ctx, bucket.Spec.Name, minio.RemoveBucketOptions{ForceDelete: true})
	if merr, ok := err.(minio.ErrorResponse); ok && merr.Code == "NoSuchBucket" {
		err = nil
	}
	require.NoError(err, "remove test minio bucket")
	err = ma.UpdateGroupMembers(ctx, madmin.GroupAddRemove{Group: builtinGroup.Spec.Name, IsRemove: true})
	if merr, ok := err.(madmin.ErrorResponse); ok && merr.Code == "XMinioAdminNoSuchGroup" {
		err = nil
	}
	// cannot delete policies until all identities are detached
	lpes, err := ma.GetLDAPPolicyEntities(ctx, madmin.PolicyEntitiesQuery{Policy: []string{policy.Spec.Name}})
	if merr, ok := err.(madmin.ErrorResponse); ok && (merr.Code == "XMinioAdminNoSuchPolicy" || merr.Code == "XMinioIAMActionNotAllowed") {
		err = nil
	}
	require.NoError(err, "list ldap entities for test minio policy")
	pes, err := ma.GetPolicyEntities(ctx, madmin.PolicyEntitiesQuery{Policy: []string{policy.Spec.Name}})
	if merr, ok := err.(madmin.ErrorResponse); ok && merr.Code == "XMinioAdminNoSuchPolicy" {
		err = nil
	}
	require.NoError(err, "list builtin entities for test minio policy")
	for _, p := range lpes.PolicyMappings {
		for _, g := range p.Groups {
			_, err = ma.DetachPolicyLDAP(ctx, madmin.PolicyAssociationReq{Policies: []string{policy.Spec.Name}, Group: g})
			require.NoError(err, "detach ldap group from test minio policy")
		}
		for _, u := range p.Users {
			_, err = ma.DetachPolicyLDAP(ctx, madmin.PolicyAssociationReq{Policies: []string{policy.Spec.Name}, User: u})
			require.NoError(err, "detach ldap user from test minio policy")
		}
	}
	for _, p := range pes.PolicyMappings {
		for _, g := range p.Groups {
			_, err = ma.DetachPolicy(ctx, madmin.PolicyAssociationReq{Policies: []string{policy.Spec.Name}, Group: g})
			require.NoError(err, "detach builtin group from test minio policy")
		}
		for _, u := range p.Users {
			_, err = ma.DetachPolicy(ctx, madmin.PolicyAssociationReq{Policies: []string{policy.Spec.Name}, User: u})
			require.NoError(err, "detach builtin user from test minio policy")
		}
	}
	err = ma.RemoveCannedPolicy(ctx, policy.Spec.Name)
	require.NoError(err, "remove test minio policy")
	err = ma.RemoveUser(ctx, builtinUser.Spec.AccessKey)
	if merr, ok := err.(madmin.ErrorResponse); ok && merr.Code == "XMinioAdminNoSuchUser" {
		err = nil
	}
	require.NoError(err, "remove test minio user")

	// delete existing identity provider
	ics, err := ma.ListIDPConfig(ctx, "ldap")
	require.NoError(err, "list minio identity providers config")
	ic := ics[0]
	r, err := ma.DeleteIDPConfig(ctx, ic.Type, ic.Name)
	require.NoError(err, "delete minio identity provider")
	if r {
		err = ma.ServiceRestartV2(ctx)
		require.NoError(err, "service restart after identity provider deletion")
		// TODO: figure out better way to detect service restart
		time.Sleep(500 * time.Millisecond)
	}

	return TestData{
		Ctx:        context.Background(),
		Kube:       k,
		KubeConfig: kc,
		Minio:      *m,
		Madmin:     *ma,
		Require:    *require,
		T:          t,
	}
}

// Configures minio to use an LDAP identity provider
func SetMinioLDAPIdentityProvider(td TestData) {
	c := []string{
		"server_addr=openldap.default.svc:389",
		"lookup_bind_dn='cn=ldap-admin,dc=example,dc=org'",
		"lookup_bind_password=ldap-admin",
		"user_dn_search_base_dn='ou=users,dc=example,dc=org'",
		"user_dn_search_filter='(&(objectClass=posixAccount)(uid=%s))'",
		"group_search_base_dn='ou=users,dc=example,dc=org'",
		"group_search_filter='(&(objectClass=groupOfNames)(member=%d))'",
		"server_insecure=on",
	}
	r, err := td.Madmin.AddOrUpdateIDPConfig(td.Ctx, "ldap", "", strings.Join(c, " "), false)
	td.Require.NoError(err, "set minio ldap identity provider")
	if r {
		td.Madmin.ServiceRestartV2(td.Ctx)
		time.Sleep(500 * time.Millisecond)
	}
}

// LogLevelHandler configures a [slog.Handler] with a log level derived from the environment.
type LogLevelHandler struct {
	slog.Handler
}

func (src *LogLevelHandler) GetLevel() slog.Level {
	l := slog.Level(100)
	if os.Getenv("VERBOSE") == "1" {
		l = slog.LevelDebug
	}
	return l
}

// Part of the [LogLevelHandler] implementation of [slog.Handler]
func (src *LogLevelHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return lvl >= src.GetLevel()
}

// Used by [LogLevelHandler.Handle] to prevent error logs from being emitted.
type DoNotLog struct{}

func (e *DoNotLog) Error() string {
	return "do not log"
}

// Part of the [LogLevelHandler] implementation of [slog.Handler]
// NOTE: [logr.Logger] will log error messages regardless of verbosity - which is why this method is required
// NOTE: [DoNotLog] is returned (vs. nil) - otherwise the log-line prefix will be emitted.
func (src *LogLevelHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= src.GetLevel() {
		return src.Handler.Handle(ctx, r)
	}
	return &DoNotLog{}
}

// Part of the [LogLevelHandler] implementation of [slog.Handler]
func (src *LogLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LogLevelHandler{Handler: src.Handler.WithAttrs(attrs)}
}

// Part of the [LogLevelHandler] implementation of [slog.Handler]
func (src *LogLevelHandler) WithGroup(name string) slog.Handler {
	return &LogLevelHandler{Handler: src.Handler.WithGroup(name)}
}

// Returns a [slogt.Option] wrapping a [slogt.Bridge]'s [slog.Handler] in a [LogLevelHandler]
func WithLogLevelHandler() slogt.Option {
	return func(b *slogt.Bridge) {
		b.Handler = &LogLevelHandler{Handler: b.Handler}
	}
}

// LogCollector wraps a [slog.Handler] and collects emitted records
type LogCollector struct {
	slog.Handler
	Records *[]slog.Record
}

// Part of the [LogCollector] implementation of [slog.Handler]
func (src *LogCollector) Handle(ctx context.Context, r slog.Record) error {
	rs := append(*src.Records, r)
	*src.Records = rs
	return src.Handler.Handle(ctx, r)
}

// Part of the [LogCollector] implementation of [slog.Handler]
func (src *LogCollector) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &LogCollector{Records: src.Records, Handler: src.Handler.WithAttrs(attrs)}
}

// Part of the [LogCollector] implementation of [slog.Handler]
func (src *LogCollector) WithGroup(name string) slog.Handler {
	return &LogCollector{Records: src.Records, Handler: src.Handler.WithGroup(name)}
}

// Returns a [slogt.Option] wrapping a [slogt.Bridge]'s [slog.Handler] in a [LogCollector]
func WithLogCollector(records *[]slog.Record) slogt.Option {
	return func(b *slogt.Bridge) {
		b.Handler = &LogCollector{Handler: b.Handler, Records: records}
	}
}

// RunOperatorUntilOptions holds options for [RunOperatorUntil]
type RunOperatorUntilOptions struct {
	operator *operator.Main
}

// RunOperatorUntilOpt defines a function that configures a [RunOperatorUntilOptions] struct
type RunOperatorUntilOpt func(d *RunOperatorUntilOptions)

// Configures [RunOperatorUntil] to use a pre-constructed operator
// See: [WaitForReconcilerError]
func RunWithOperator(o *operator.Main) RunOperatorUntilOpt {
	return func(d *RunOperatorUntilOptions) {
		d.operator = o
	}
}

// Runs operator until provided function returns [StopIteration].
func RunOperatorUntil(td TestData, rof func() error, ofs ...RunOperatorUntilOpt) {
	td.T.Helper()

	// obtain options
	opts := &RunOperatorUntilOptions{}
	for _, of := range ofs {
		of(opts)
	}

	o := opts.operator
	var err error
	if o == nil {
		// create operator if not defined
		l := slogt.New(td.T, slogt.Text(), WithLogLevelHandler())
		o, err = operator.New(&operator.Opts{KubeConfig: td.KubeConfig, Logger: l, MinioOperatorNamespace: "default"})
		td.Require.NoError(err, "create operator")
	}

	// start operator in background
	var oerr error
	sctx, cancel := context.WithCancel(td.Ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		oerr = o.Run(sctx)
	}()

	// poll function until function returns error
	// NOTE: StopIteration is an error
	to := 30 * time.Second
	st := time.Now()
	for {
		ct := time.Now()
		if ct.Sub(st) > to {
			err = fmt.Errorf("timed out waiting for end condition")
			break
		}
		err := rof()
		if err != nil {
			break
		}
	}

	// stop the operator
	cancel()
	wg.Wait()

	// if an error was thrown while polling,
	if err != nil {
		td.Require.ErrorIs(err, StopIteration{}, "within run operator function")
	}
	td.Require.NoError(oerr, "during operator run")
}

// StopIteration signals to [RunOperatorUntil] that it should stop polling kubernetes for state changes.
type StopIteration struct{}

// Returns an error string for [StopIteration]
func (si StopIteration) Error() string {
	return "stop iteration"
}

// Waits for the provided resource to be deleted
func WaitForDelete(td TestData, o client.Object) {
	td.T.Helper()

	RunOperatorUntil(td, func() error {
		err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(o), o)
		if err == nil {
			return nil
		}
		if apierrors.IsNotFound(err) {
			err = nil
		}
		td.Require.NoError(err, "fetch object while waiting for object to be deleted")
		return StopIteration{}
	})
}

// Ensures that over a set number of iterations, the operator isn't changing the [client.Object]'s ResourceVersion.
// This is designed to catch logic flaws within the operator that result in spurious updates to an underlying resource.
func WaitForStableResourceVersion(td TestData, o client.Object) {
	td.T.Helper()

	c := 0
	t := 2
	lrv := ""
	i := 0
	mi := 10

	RunOperatorUntil(td, func() error {
		err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(o), o)
		td.Require.NoError(err, "fetch object while waiting for stable resource version")
		rv := o.GetResourceVersion()
		if lrv != rv {
			c += 1
			lrv = rv
		}
		i += 1
		if i < mi {
			return nil
		}
		return StopIteration{}
	})

	td.Require.True(c < t, "resource change threshold reached")
}

// Collects reconciler errors and passes them to the callback - allowing unit tests to test for certain failures.
// Accomplishes this by attaching a handler to the operator that collects log records and processes them for an 'err' attribute.
func WaitForReconcilerError(td TestData, cb func(err error) error) {
	rs := []slog.Record{}
	prs := &rs

	l := slogt.New(td.T, slogt.Text(), WithLogLevelHandler(), WithLogCollector(&rs))
	o, err := operator.New(&operator.Opts{KubeConfig: td.KubeConfig, Logger: l, MinioOperatorNamespace: "default"})
	td.Require.NoError(err, "create operator")

	i := 0
	RunOperatorUntil(td, func() error {
		for i < len(*prs) {
			r := (*prs)[i]
			i = i + 1

			if r.Message != "Reconciler error" {
				// not a reconciler error - ignore
				continue
			}

			// find the error as an attribute of the record
			var lerr error
			r.Attrs(func(a slog.Attr) bool {
				if a.Key != "err" {
					// not an error attribute - ignore
					return true
				}
				aerr, ok := a.Value.Any().(error)
				if !ok {
					return true
				}
				lerr = aerr
				return false
			})
			if lerr == nil {
				// err was not found
				continue
			}

			// invoke the callback with the found error
			err := cb(lerr)
			if err != nil {
				return err
			}
		}
		return nil
	}, RunWithOperator(o))
}

func TestMinioBucket(t *testing.T) {
	createBucket := func(td TestData) *v1.MinioBucket {
		td.T.Helper()

		b := bucket.DeepCopy()
		err := td.Kube.Create(td.Ctx, b)
		td.Require.NoError(err, "create bucket object")

		return b
	}

	waitForReconcile := func(td TestData, b *v1.MinioBucket) {
		RunOperatorUntil(td, func() error {
			err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(b), b)
			td.Require.NoError(err, "wait for bucket reconcile")
			if b.Status.CurrentSpec == nil {
				return nil
			}
			if !reflect.DeepEqual(*b.Status.CurrentSpec, b.Spec) {
				return nil
			}
			return StopIteration{}
		})
	}

	t.Run("creates a minio bucket", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		waitForReconcile(td, b)

		be, err := td.Minio.BucketExists(td.Ctx, b.Spec.Name)
		td.Require.NoError(err, "check if bucket exists")
		td.Require.True(be, "check if bucket exists")
	})

	t.Run("does not create a minio bucket if one exists", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		err := td.Minio.MakeBucket(td.Ctx, b.Spec.Name, minio.MakeBucketOptions{})
		td.Require.NoError(err, "create minio bucket")

		WaitForReconcilerError(td, func(err error) error {
			merr, ok := err.(minio.ErrorResponse)
			if !ok {
				return nil
			}
			if merr.Code != "BucketAlreadyOwnedByYou" {
				return nil
			}
			if merr.BucketName != b.Spec.Name {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("migrates an existing bucket if migrate set to true", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		err := td.Minio.MakeBucket(td.Ctx, b.Spec.Name, minio.MakeBucketOptions{})
		td.Require.NoError(err, "create minio bucket")
		b.Spec.Migrate = true
		err = td.Kube.Update(td.Ctx, b)
		td.Require.NoError(err, "update bucket resource")

		waitForReconcile(td, b)
		td.Require.False(b.Spec.Migrate, "migrate unset on reconcile")
	})

	t.Run("deletes a minio bucket", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		waitForReconcile(td, b)

		err := td.Kube.Delete(td.Ctx, b)
		td.Require.NoError(err, "delete bucket object")
		WaitForDelete(td, b)

		be, err := td.Minio.BucketExists(td.Ctx, b.Spec.Name)
		td.Require.NoError(err, "check if bucket does not exist")
		td.Require.False(be)
	})

	t.Run("deletes resource even when minio bucket missing", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		waitForReconcile(td, b)

		err := td.Minio.RemoveBucketWithOptions(td.Ctx, b.Spec.Name, minio.RemoveBucketOptions{ForceDelete: true})
		td.Require.NoError(err, "delete bucket from minio")

		err = td.Kube.Delete(td.Ctx, b)
		td.Require.NoError(err, "delete bucket object")
		WaitForDelete(td, b)
	})

	t.Run("recreates minio bucket if deleted in background", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		waitForReconcile(td, b)

		err := td.Minio.RemoveBucketWithOptions(td.Ctx, b.Spec.Name, minio.RemoveBucketOptions{ForceDelete: true})
		td.Require.NoError(err, "delete minio bucket resource")

		RunOperatorUntil(td, func() error {
			be, err := td.Minio.BucketExists(td.Ctx, b.Spec.Name)
			td.Require.NoError(err, "check if bucket created")
			if !be {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("ensure resource version stabilizes", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		waitForReconcile(td, b)

		WaitForStableResourceVersion(td, b)
	})

	t.Run("does not force delete bucket if deletion policy IfEmpty", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		waitForReconcile(td, b)

		data := "hello world"
		_, err := td.Minio.PutObject(td.Ctx, b.Spec.Name, "testing-object", strings.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
		td.Require.NoError(err, "put data in minio bucket")

		err = td.Kube.Delete(td.Ctx, b)
		td.Require.NoError(err, "delete bucket object")

		WaitForReconcilerError(td, func(err error) error {
			merr, ok := err.(minio.ErrorResponse)
			if !ok {
				return nil
			}
			if merr.Code != "BucketNotEmpty" {
				return nil
			}
			if merr.BucketName != b.Spec.Name {
				return nil
			}
			return StopIteration{}
		})

		err = td.Minio.RemoveObject(td.Ctx, b.Spec.Name, "testing-object", minio.RemoveObjectOptions{ForceDelete: true})
		td.Require.NoError(err, "remove data from minio bucket")

		WaitForDelete(td, b)
	})

	t.Run("force delete bucket if deletion policy Always", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		waitForReconcile(td, b)

		b.Spec.DeletionPolicy = v1.MinioBucketDeletionPolicyAlways
		err := td.Kube.Update(td.Ctx, b)
		td.Require.NoError(err, "update bucket object deletion policy")
		waitForReconcile(td, b)

		data := "hello world"
		_, err = td.Minio.PutObject(td.Ctx, b.Spec.Name, "testing-object", strings.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
		td.Require.NoError(err, "put data in minio bucket")

		err = td.Kube.Delete(td.Ctx, b)
		td.Require.NoError(err, "delete bucket object")

		WaitForDelete(td, b)
	})

	t.Run("set bucket versioning config on create", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		b.Spec.Versioning = &v1.MinioBucketVersioningConfiguration{
			Status: "Enabled",
		}
		err := td.Kube.Update(td.Ctx, b)
		td.Require.NoError(err, "update bucket")

		waitForReconcile(td, b)

		vc, err := td.Minio.GetBucketVersioning(td.Ctx, b.Spec.Name)
		td.Require.NoError(err, "get minio bucket")

		td.Require.True(vc.Enabled())
	})

	t.Run("set bucket versioning config on update", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		waitForReconcile(td, b)

		b.Spec.Versioning = &v1.MinioBucketVersioningConfiguration{
			Status: "Enabled",
		}
		err := td.Kube.Update(td.Ctx, b)
		td.Require.NoError(err, "update bucket")

		waitForReconcile(td, b)

		vc, err := td.Minio.GetBucketVersioning(td.Ctx, b.Spec.Name)
		td.Require.NoError(err, "get minio bucket")

		td.Require.True(vc.Enabled())
	})

	t.Run("corrects bucket versioning config drift", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		b.Spec.Versioning = &v1.MinioBucketVersioningConfiguration{
			Status: "Enabled",
		}
		err := td.Kube.Update(td.Ctx, b)
		td.Require.NoError(err, "update bucket")
		waitForReconcile(td, b)

		err = td.Minio.SetBucketVersioning(td.Ctx, b.Spec.Name, minio.BucketVersioningConfiguration{
			Status: "Suspended",
		})
		td.Require.NoError(err, "update minio bucket")

		RunOperatorUntil(td, func() error {
			vc, err := td.Minio.GetBucketVersioning(td.Ctx, b.Spec.Name)
			if err != nil {
				return err
			}
			if !vc.Enabled() {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("set bucket lifecycle config on create", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)

		b.Spec.Lifecycle = &v1.MinioBucketLifecycleConfiguration{
			Rules: []v1.MinioBucketLifecycleRule{{
				Expiration: v1.MinioBucketLifecycleExpiration{
					Days: 1,
				},
				Status: "Enabled",
			}},
		}
		err := td.Kube.Update(td.Ctx, b)
		td.Require.NoError(err, "update bucket")

		waitForReconcile(td, b)

		lc, err := td.Minio.GetBucketLifecycle(td.Ctx, b.Spec.Name)
		td.Require.NoError(err, "get minio bucket")

		td.Require.True(lc.Rules[0].Expiration.Days == 1)
	})

	t.Run("set bucket lifecycle config on update", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)
		waitForReconcile(td, b)

		b.Spec.Lifecycle = &v1.MinioBucketLifecycleConfiguration{
			Rules: []v1.MinioBucketLifecycleRule{{
				Expiration: v1.MinioBucketLifecycleExpiration{
					Days: 1,
				},
				Status: "Enabled",
			}},
		}
		err := td.Kube.Update(td.Ctx, b)
		td.Require.NoError(err, "update bucket")
		waitForReconcile(td, b)

		lc, err := td.Minio.GetBucketLifecycle(td.Ctx, b.Spec.Name)
		td.Require.NoError(err, "get minio bucket")

		td.Require.True(lc.Rules[0].Expiration.Days == 1)
	})

	t.Run("corrects bucket lifecycle config drift", func(t *testing.T) {
		td := Setup(t)

		b := createBucket(td)

		b.Spec.Lifecycle = &v1.MinioBucketLifecycleConfiguration{
			Rules: []v1.MinioBucketLifecycleRule{{
				Expiration: v1.MinioBucketLifecycleExpiration{
					Days: 1,
				},
				Status: "Enabled",
			}},
		}
		err := td.Kube.Update(td.Ctx, b)
		td.Require.NoError(err, "update bucket")

		waitForReconcile(td, b)

		lc := lifecycle.NewConfiguration()
		lc.Rules = []lifecycle.Rule{{
			Expiration: lifecycle.Expiration{
				Days: 1,
			},
			Status: "Disabled",
		}}
		err = td.Minio.SetBucketLifecycle(td.Ctx, b.Spec.Name, lc)
		td.Require.NoError(err, "update minio bucket")

		RunOperatorUntil(td, func() error {
			lc, err := td.Minio.GetBucketLifecycle(td.Ctx, b.Spec.Name)
			if err != nil {
				return err
			}
			if lc.Rules[0].Status == "Disabled" {
				return nil
			}
			return StopIteration{}
		})
	})
}

func TestMinioGroup(t *testing.T) {
	createGroup := func(td TestData) *v1.MinioGroup {
		td.T.Helper()

		g := builtinGroup.DeepCopy()
		err := td.Kube.Create(td.Ctx, g)
		td.Require.NoError(err, "create group object")

		return g
	}

	waitForReconcile := func(td TestData, g *v1.MinioGroup) {
		RunOperatorUntil(td, func() error {
			err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(g), g)
			td.Require.NoError(err, "waiting for group reconcile")
			if g.Status.CurrentSpec == nil {
				return nil
			}
			if !reflect.DeepEqual(g.Spec, *g.Status.CurrentSpec) {
				return nil
			}
			return StopIteration{}
		})
	}

	t.Run("creates a minio group", func(t *testing.T) {
		td := Setup(t)

		g := createGroup(td)
		waitForReconcile(td, g)

		_, err := td.Madmin.GetGroupDescription(td.Ctx, g.Spec.Name)
		td.Require.NoError(err, "check if group exists")
	})

	t.Run("does not create a minio group if one exists", func(t *testing.T) {
		td := Setup(t)

		g := createGroup(td)
		err := td.Madmin.UpdateGroupMembers(td.Ctx, madmin.GroupAddRemove{Group: g.Spec.Name})
		td.Require.NoError(err, "create minio group")

		WaitForReconcilerError(td, func(err error) error {
			if err.Error() != fmt.Sprintf("group %s already exists", g.Spec.Name) {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("migrates an existing group if migrate set to true", func(t *testing.T) {
		td := Setup(t)

		g := createGroup(td)
		err := td.Madmin.UpdateGroupMembers(td.Ctx, madmin.GroupAddRemove{Group: g.Spec.Name})
		td.Require.NoError(err, "create minio group")
		g.Spec.Migrate = true
		err = td.Kube.Update(td.Ctx, g)
		td.Require.NoError(err, "update group resource")

		waitForReconcile(td, g)
		td.Require.False(g.Spec.Migrate, "migrate unset on reconcile")
	})

	t.Run("deletes a minio group", func(t *testing.T) {
		td := Setup(t)

		g := createGroup(td)
		waitForReconcile(td, g)

		err := td.Kube.Delete(td.Ctx, g)
		td.Require.NoError(err, "delete group object")
		WaitForDelete(td, g)

		_, err = td.Madmin.GetGroupDescription(td.Ctx, g.Spec.Name)
		merr := &madmin.ErrorResponse{}
		td.Require.ErrorAs(err, merr, "check expected error type")
		td.Require.Equal(merr.Code, "XMinioAdminNoSuchGroup", "check expected error code")
	})

	t.Run("deletes resource even when minio group missing", func(t *testing.T) {
		td := Setup(t)

		g := createGroup(td)
		waitForReconcile(td, g)

		err := td.Madmin.UpdateGroupMembers(td.Ctx, madmin.GroupAddRemove{Group: g.Spec.Name, IsRemove: true})
		td.Require.NoError(err, "delete minio group resource")

		err = td.Kube.Delete(td.Ctx, g)
		td.Require.NoError(err, "delete group object")
		WaitForDelete(td, g)
	})

	t.Run("recreates minio group if deleted in background", func(t *testing.T) {
		td := Setup(t)

		g := createGroup(td)
		waitForReconcile(td, g)

		err := td.Madmin.UpdateGroupMembers(td.Ctx, madmin.GroupAddRemove{Group: g.Spec.Name, IsRemove: true})
		td.Require.NoError(err, "delete minio group resource")

		RunOperatorUntil(td, func() error {
			_, err := td.Madmin.GetGroupDescription(td.Ctx, g.Spec.Name)
			if merr, ok := err.(madmin.ErrorResponse); ok && merr.Code == "XMinioAdminNoSuchGroup" {
				return nil
			}
			td.Require.NoError(err, "check if group created")
			return StopIteration{}
		})
	})

	t.Run("ensure resource version stabilizes", func(t *testing.T) {
		td := Setup(t)

		g := createGroup(td)
		waitForReconcile(td, g)

		WaitForStableResourceVersion(td, g)
	})
}

func TestMinioGroupBinding(t *testing.T) {
	createGroupBinding := func(td TestData) *v1.MinioGroupBinding {
		td.T.Helper()

		us := builtinUserSecret.DeepCopy()
		err := td.Kube.Create(td.Ctx, us)
		td.Require.NoError(err, "create user secret")
		u := builtinUser.DeepCopy()
		err = td.Kube.Create(td.Ctx, u)
		td.Require.NoError(err, "create user")
		g := builtinGroup.DeepCopy()
		err = td.Kube.Create(td.Ctx, g)
		td.Require.NoError(err, "create group object")
		gb := builtinGroupToBuiltinUser.DeepCopy()
		err = td.Kube.Create(td.Ctx, gb)
		td.Require.NoError(err, "create group binding")

		return gb
	}

	waitForReconcile := func(td TestData, gb *v1.MinioGroupBinding) {
		RunOperatorUntil(td, func() error {
			err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(gb), gb)
			td.Require.NoError(err, "waiting for group binding reconcile")
			if gb.Status.CurrentSpec == nil {
				return nil
			}
			if !reflect.DeepEqual(gb.Spec, *gb.Status.CurrentSpec) {
				return nil
			}
			return StopIteration{}
		})
	}

	t.Run("creates a minio group binding", func(t *testing.T) {
		td := Setup(t)

		gb := createGroupBinding(td)
		waitForReconcile(td, gb)

		gd, err := td.Madmin.GetGroupDescription(td.Ctx, gb.Spec.Group)
		td.Require.NoError(err, "check if user is group member")
		td.Require.True(slices.Contains(gd.Members, gb.Spec.User), "user not member of group")
	})

	t.Run("does not create a minio group binding if one exists", func(t *testing.T) {
		td := Setup(t)

		gb := createGroupBinding(td)
		waitForReconcile(td, gb)

		err := td.Kube.Delete(td.Ctx, gb)
		td.Require.NoError(err, "delete minio group binding resource")
		WaitForDelete(td, gb)

		gb = builtinGroupToBuiltinUser.DeepCopy()
		err = td.Kube.Create(td.Ctx, gb)
		td.Require.NoError(err, "create minio group binding resource")
		err = td.Madmin.UpdateGroupMembers(td.Ctx, madmin.GroupAddRemove{Group: gb.Spec.Group, Members: []string{gb.Spec.User}})
		td.Require.NoError(err, "create minio group binding")

		WaitForReconcilerError(td, func(err error) error {
			if err.Error() != fmt.Sprintf("user %s already member of group %s", gb.Spec.User, gb.Spec.Group) {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("migrates an existing group if migrate set to true", func(t *testing.T) {
		td := Setup(t)

		gb := createGroupBinding(td)
		waitForReconcile(td, gb)

		err := td.Kube.Delete(td.Ctx, gb)
		td.Require.NoError(err, "delete minio group binding resource")
		WaitForDelete(td, gb)

		gb = builtinGroupToBuiltinUser.DeepCopy()
		gb.Spec.Migrate = true
		err = td.Kube.Create(td.Ctx, gb)
		td.Require.NoError(err, "create minio group binding resource")
		err = td.Madmin.UpdateGroupMembers(td.Ctx, madmin.GroupAddRemove{Group: gb.Spec.Group, Members: []string{gb.Spec.User}})
		td.Require.NoError(err, "create minio group binding")

		waitForReconcile(td, gb)
		td.Require.False(gb.Spec.Migrate, "migrate unset on reconcile")
	})

	t.Run("deletes a minio group binding", func(t *testing.T) {
		td := Setup(t)

		gb := createGroupBinding(td)
		waitForReconcile(td, gb)

		err := td.Kube.Delete(td.Ctx, gb)
		td.Require.NoError(err, "delete group binding object")
		WaitForDelete(td, gb)

		gd, err := td.Madmin.GetGroupDescription(td.Ctx, gb.Spec.Group)
		td.Require.NoError(err, "check if user is not group member")
		td.Require.False(slices.Contains(gd.Members, gb.Spec.User), "user not member of group")
	})

	t.Run("recreates minio group binding on change", func(t *testing.T) {
		td := Setup(t)

		gb := createGroupBinding(td)
		waitForReconcile(td, gb)

		bu2 := builtinUser2.DeepCopy()
		err := td.Kube.Create(td.Ctx, bu2)
		td.Require.NoError(err, "create second user")
		gb.Spec.User = bu2.Spec.AccessKey
		waitForReconcile(td, gb)

		gd, err := td.Madmin.GetGroupDescription(td.Ctx, gb.Spec.Group)
		td.Require.NoError(err, "check if group membership updated")
		td.Require.True(slices.Contains(gd.Members, gb.Spec.User))
	})

	t.Run("recreates minio group binding if deleted in background", func(t *testing.T) {
		td := Setup(t)

		gb := createGroupBinding(td)
		waitForReconcile(td, gb)

		err := td.Madmin.UpdateGroupMembers(td.Ctx, madmin.GroupAddRemove{Group: gb.Spec.Group, Members: []string{gb.Spec.User}, IsRemove: true})
		td.Require.NoError(err, "remove group member")

		RunOperatorUntil(td, func() error {
			gd, err := td.Madmin.GetGroupDescription(td.Ctx, gb.Spec.Group)
			td.Require.NoError(err, "check if group membership updated")
			if len(gd.Members) == 0 {
				return nil
			}
			return StopIteration{}
		})

		gd, err := td.Madmin.GetGroupDescription(td.Ctx, gb.Spec.Group)
		td.Require.NoError(err, "check if group membership updated")
		td.Require.True(slices.Contains(gd.Members, gb.Spec.User))
	})

	t.Run("ensure resource version stabilizes", func(t *testing.T) {
		td := Setup(t)

		gb := createGroupBinding(td)
		waitForReconcile(td, gb)

		WaitForStableResourceVersion(td, gb)
	})

	t.Run("can still delete a minio group binding when user deleted", func(t *testing.T) {
		td := Setup(t)

		gb := createGroupBinding(td)
		waitForReconcile(td, gb)

		u := builtinUser.DeepCopy()
		err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(u), u)
		td.Require.NoError(err, "get user object")
		err = td.Kube.Delete(td.Ctx, u)
		td.Require.NoError(err, "delete user object")
		WaitForDelete(td, u)

		err = td.Kube.Delete(td.Ctx, gb)
		td.Require.NoError(err, "delete group binding object")
		WaitForDelete(td, gb)

		gd, err := td.Madmin.GetGroupDescription(td.Ctx, gb.Spec.Group)
		td.Require.NoError(err, "check if user is not group member")
		td.Require.False(slices.Contains(gd.Members, gb.Spec.User), "user not member of group")
	})
}

func TestMinioPolicy(t *testing.T) {
	createPolicy := func(td TestData) *v1.MinioPolicy {
		td.T.Helper()

		p := policy.DeepCopy()
		err := td.Kube.Create(td.Ctx, p)
		td.Require.NoError(err, "create policy object")

		return p
	}

	waitForReconcile := func(td TestData, p *v1.MinioPolicy) {
		RunOperatorUntil(td, func() error {
			err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(p), p)
			td.Require.NoError(err, "waiting for policy reconcile")
			if p.Status.CurrentSpec == nil {
				return nil
			}
			if !reflect.DeepEqual(p.Spec, *p.Status.CurrentSpec) {
				return nil
			}
			return StopIteration{}
		})
	}

	t.Run("creates a minio policy", func(t *testing.T) {
		td := Setup(t)

		p := createPolicy(td)
		waitForReconcile(td, p)

		_, err := td.Madmin.InfoCannedPolicyV2(td.Ctx, p.Spec.Name)
		td.Require.NoError(err, "check if policy exists")
	})

	t.Run("does not create a minio policy if one exists", func(t *testing.T) {
		td := Setup(t)

		p := createPolicy(td)
		pb, err := json.Marshal(map[string]any{"Version": p.Spec.Version, "Statement": p.Spec.Statement})
		td.Require.NoError(err, "marshal minio policy")
		err = td.Madmin.AddCannedPolicy(td.Ctx, p.Spec.Name, pb)
		td.Require.NoError(err, "create minio policy")

		WaitForReconcilerError(td, func(err error) error {
			if err.Error() != fmt.Sprintf("policy %s already exists", p.Spec.Name) {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("migrates an existing policy if migrate set to true", func(t *testing.T) {
		td := Setup(t)

		p := createPolicy(td)
		p.Spec.Migrate = true
		err := td.Kube.Update(td.Ctx, p)
		td.Require.NoError(err, "update minio policy resource")
		pb, err := json.Marshal(map[string]any{"Version": p.Spec.Version, "Statement": p.Spec.Statement})
		td.Require.NoError(err, "marshal minio policy")
		err = td.Madmin.AddCannedPolicy(td.Ctx, p.Spec.Name, pb)
		td.Require.NoError(err, "create minio policy")

		waitForReconcile(td, p)
		td.Require.False(p.Spec.Migrate, "migrate unset on reconcile")
	})

	t.Run("deletes a minio policy", func(t *testing.T) {
		td := Setup(t)

		p := createPolicy(td)
		waitForReconcile(td, p)

		err := td.Kube.Delete(td.Ctx, p)
		td.Require.NoError(err, "delete policy object")
		WaitForDelete(td, p)

		_, err = td.Madmin.InfoCannedPolicyV2(td.Ctx, p.Spec.Name)
		merr := &madmin.ErrorResponse{}
		td.Require.ErrorAs(err, merr, "check expected error type")
		td.Require.Equal(merr.Code, "XMinioAdminNoSuchPolicy", "check expected error code")
	})

	t.Run("deletes resource even when minio policy missing", func(t *testing.T) {
		td := Setup(t)

		p := createPolicy(td)
		waitForReconcile(td, p)

		err := td.Madmin.RemoveCannedPolicy(td.Ctx, p.Spec.Name)
		td.Require.NoError(err, "delete minio policy resource")

		err = td.Kube.Delete(td.Ctx, p)
		td.Require.NoError(err, "delete policy object")
		WaitForDelete(td, p)
	})

	t.Run("recreates minio policy if deleted in background", func(t *testing.T) {
		td := Setup(t)

		p := createPolicy(td)
		waitForReconcile(td, p)

		err := td.Madmin.RemoveCannedPolicy(td.Ctx, p.Spec.Name)
		td.Require.NoError(err, "delete minio policy resource")

		RunOperatorUntil(td, func() error {
			_, err := td.Madmin.InfoCannedPolicyV2(td.Ctx, p.Spec.Name)
			if merr, ok := err.(madmin.ErrorResponse); ok && merr.Code == "XMinioAdminNoSuchPolicy" {
				return nil
			}
			td.Require.NoError(err, "check if policy created")
			return StopIteration{}
		})
	})

	t.Run("updates minio policy when spec changes", func(t *testing.T) {
		td := Setup(t)

		p := createPolicy(td)
		waitForReconcile(td, p)

		p.Spec.Statement[0].Effect = "Deny"
		err := td.Kube.Update(td.Ctx, p)
		td.Require.NoError(err, "update minio policy statement")
		waitForReconcile(td, p)
	})

	t.Run("updates minio policy when spec differs from remote", func(t *testing.T) {
		td := Setup(t)

		p := createPolicy(td)
		waitForReconcile(td, p)

		cp := p.DeepCopy()
		cp.Spec.Statement[0].Effect = "Deny"
		pd, err := json.Marshal(map[string]any{
			"statement": cp.Spec.Statement,
			"version":   cp.Spec.Version,
		})
		td.Require.NoError(err, "marshal new minio policy")
		err = td.Madmin.AddCannedPolicy(td.Ctx, p.Spec.Name, pd)
		td.Require.NoError(err, "update remote minio policy statement")

		RunOperatorUntil(td, func() error {
			mp, err := td.Madmin.InfoCannedPolicyV2(td.Ctx, p.Spec.Name)
			td.Require.NoError(err, "check if remote minio policy updated")
			mps := &v1.MinioPolicySpec{}
			err = json.Unmarshal(mp.Policy, mps)
			td.Require.NoError(err, "unmarshal remote minio policy")
			if mps.Statement[0].Effect == "Deny" {
				return nil
			}
			return StopIteration{}
		})
	})
}

func TestMinioPolicyBinding(t *testing.T) {
	createBuiltinUserPolicyBinding := func(td TestData) *v1.MinioPolicyBinding {
		td.T.Helper()

		us := builtinUserSecret.DeepCopy()
		err := td.Kube.Create(td.Ctx, us)
		td.Require.NoError(err, "create user secret object")
		u := builtinUser.DeepCopy()
		err = td.Kube.Create(td.Ctx, u)
		td.Require.NoError(err, "create user object")
		p := policy.DeepCopy()
		err = td.Kube.Create(td.Ctx, p)
		td.Require.NoError(err, "create policy object")
		pb := policyToBuiltinUser.DeepCopy()
		err = td.Kube.Create(td.Ctx, pb)
		td.Require.NoError(err, "create policy binding object")

		return pb
	}

	createBuiltinGroupPolicyBinding := func(td TestData) *v1.MinioPolicyBinding {
		td.T.Helper()

		g := builtinGroup.DeepCopy()
		err := td.Kube.Create(td.Ctx, g)
		td.Require.NoError(err, "create group object")
		p := policy.DeepCopy()
		err = td.Kube.Create(td.Ctx, p)
		td.Require.NoError(err, "create policy object")
		pb := policyToBuiltinGroup.DeepCopy()
		err = td.Kube.Create(td.Ctx, pb)
		td.Require.NoError(err, "create policy binding object")

		return pb
	}

	createLdapGroupPolicyBinding := func(td TestData) *v1.MinioPolicyBinding {
		td.T.Helper()

		p := policy.DeepCopy()
		err := td.Kube.Create(td.Ctx, p)
		td.Require.NoError(err, "create policy object")
		pb := policyToLdapGroup.DeepCopy()
		err = td.Kube.Create(td.Ctx, pb)
		td.Require.NoError(err, "create policy binding object")

		return pb
	}

	createLdapUserPolicyBinding := func(td TestData) *v1.MinioPolicyBinding {
		td.T.Helper()

		p := policy.DeepCopy()
		err := td.Kube.Create(td.Ctx, p)
		td.Require.NoError(err, "create policy object")
		pb := policyToLdapUser.DeepCopy()
		err = td.Kube.Create(td.Ctx, pb)
		td.Require.NoError(err, "create policy binding object")

		return pb
	}

	waitForReconcile := func(td TestData, pb *v1.MinioPolicyBinding) {
		RunOperatorUntil(td, func() error {
			err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(pb), pb)
			td.Require.NoError(err, "waiting for policy binding reconcile")
			if pb.Status.CurrentSpec == nil {
				return nil
			}
			if !reflect.DeepEqual(pb.Spec, *pb.Status.CurrentSpec) {
				return nil
			}
			return StopIteration{}
		})
	}

	t.Run("creates a builtin group minio policy binding", func(t *testing.T) {
		td := Setup(t)

		pb := createBuiltinGroupPolicyBinding(td)
		waitForReconcile(td, pb)

		pes, err := td.Madmin.GetPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.True(slices.Contains(pes.PolicyMappings[0].Groups, pb.Spec.Group.Builtin))
	})

	t.Run("creates a builtin user minio policy binding", func(t *testing.T) {
		td := Setup(t)

		pb := createBuiltinUserPolicyBinding(td)
		waitForReconcile(td, pb)

		pes, err := td.Madmin.GetPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.True(slices.Contains(pes.PolicyMappings[0].Users, pb.Spec.User.Builtin))
	})

	t.Run("does not create a builtin user minio policy binding if one exists", func(t *testing.T) {
		td := Setup(t)

		pb := createBuiltinUserPolicyBinding(td)
		waitForReconcile(td, pb)
		err := td.Kube.Delete(td.Ctx, pb)
		td.Require.NoError(err, "delete policy binding resource")
		WaitForDelete(td, pb)
		_, err = td.Madmin.AttachPolicy(td.Ctx, madmin.PolicyAssociationReq{Policies: []string{pb.Spec.Policy}, User: pb.Spec.User.Builtin})
		td.Require.NoError(err, "create policy binding")
		pb = policyToBuiltinUser.DeepCopy()
		err = td.Kube.Create(td.Ctx, pb)
		td.Require.NoError(err, "create policy binding resource")

		WaitForReconcilerError(td, func(err error) error {
			merr, ok := err.(madmin.ErrorResponse)
			if !ok {
				return nil
			}
			if merr.Code != "XMinioAdminPolicyChangeAlreadyApplied" {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("migrates an existing builtin user policy binding if migrate set to true", func(t *testing.T) {
		td := Setup(t)

		pb := createBuiltinUserPolicyBinding(td)
		waitForReconcile(td, pb)
		err := td.Kube.Delete(td.Ctx, pb)
		td.Require.NoError(err, "delete policy binding resource")
		WaitForDelete(td, pb)
		_, err = td.Madmin.AttachPolicy(td.Ctx, madmin.PolicyAssociationReq{Policies: []string{pb.Spec.Policy}, User: pb.Spec.User.Builtin})
		td.Require.NoError(err, "create policy binding")
		pb = policyToBuiltinUser.DeepCopy()
		pb.Spec.Migrate = true
		err = td.Kube.Create(td.Ctx, pb)
		td.Require.NoError(err, "create policy binding resource")

		waitForReconcile(td, pb)
		td.Require.False(pb.Spec.Migrate, "migrate unset on reconcile")
	})

	t.Run("creates an ldap group minio policy binding", func(t *testing.T) {
		td := Setup(t)
		SetMinioLDAPIdentityProvider(td)

		pb := createLdapGroupPolicyBinding(td)
		waitForReconcile(td, pb)

		pes, err := td.Madmin.GetLDAPPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.True(slices.Contains(pes.PolicyMappings[0].Groups, pb.Spec.Group.Ldap))
	})

	t.Run("creates an ldap user minio policy binding", func(t *testing.T) {
		td := Setup(t)
		SetMinioLDAPIdentityProvider(td)

		pb := createLdapUserPolicyBinding(td)
		waitForReconcile(td, pb)

		pes, err := td.Madmin.GetLDAPPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.True(slices.Contains(pes.PolicyMappings[0].Users, pb.Spec.User.Ldap))
	})

	t.Run("deletes a builtin group minio policy binding", func(t *testing.T) {
		td := Setup(t)

		pb := createBuiltinGroupPolicyBinding(td)
		waitForReconcile(td, pb)

		err := td.Kube.Delete(td.Ctx, pb)
		td.Require.NoError(err, "delete policy binding object")
		WaitForDelete(td, pb)

		pes, err := td.Madmin.GetPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.Len(pes.PolicyMappings, 0, "policy has no bindings")
	})

	t.Run("deletes a builtin user minio policy binding", func(t *testing.T) {
		td := Setup(t)

		pb := createBuiltinUserPolicyBinding(td)
		waitForReconcile(td, pb)

		err := td.Kube.Delete(td.Ctx, pb)
		td.Require.NoError(err, "delete policy binding object")
		WaitForDelete(td, pb)

		pes, err := td.Madmin.GetPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.Len(pes.PolicyMappings, 0, "policy has no bindings")
	})

	t.Run("deletes an ldap group minio policy binding", func(t *testing.T) {
		td := Setup(t)
		SetMinioLDAPIdentityProvider(td)

		pb := createLdapGroupPolicyBinding(td)
		waitForReconcile(td, pb)

		err := td.Kube.Delete(td.Ctx, pb)
		td.Require.NoError(err, "delete policy binding object")
		WaitForDelete(td, pb)

		pes, err := td.Madmin.GetLDAPPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.Len(pes.PolicyMappings, 0, "policy has no bindings")
	})

	t.Run("deletes an ldap user minio policy binding", func(t *testing.T) {
		td := Setup(t)
		SetMinioLDAPIdentityProvider(td)

		pb := createLdapUserPolicyBinding(td)
		waitForReconcile(td, pb)

		err := td.Kube.Delete(td.Ctx, pb)
		td.Require.NoError(err, "delete policy binding object")
		WaitForDelete(td, pb)

		pes, err := td.Madmin.GetLDAPPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.Len(pes.PolicyMappings, 0, "policy has no bindings")
	})

	t.Run("recreates minio policy binding if deleted in background", func(t *testing.T) {
		td := Setup(t)
		SetMinioLDAPIdentityProvider(td)

		pb := createLdapUserPolicyBinding(td)
		waitForReconcile(td, pb)

		_, err := td.Madmin.DetachPolicyLDAP(td.Ctx, madmin.PolicyAssociationReq{Policies: []string{pb.Spec.Policy}, User: pb.Spec.User.Ldap})
		td.Require.NoError(err, "detach ldap policy")
		RunOperatorUntil(td, func() error {
			pes, err := td.Madmin.GetLDAPPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
			td.Require.NoError(err, "check if policy entities updated")
			if len(pes.PolicyMappings) == 0 {
				return nil
			}
			return StopIteration{}
		})

		pes, err := td.Madmin.GetLDAPPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy entities updated")
		td.Require.Contains(pes.PolicyMappings[0].Users, pb.Spec.User.Ldap, "check if policy entities contain user")
	})

	t.Run("recreates minio policy binding if deleted in background (ldap user not fqdn)", func(t *testing.T) {
		td := Setup(t)
		SetMinioLDAPIdentityProvider(td)

		p := policy.DeepCopy()
		err := td.Kube.Create(td.Ctx, p)
		td.Require.NoError(err, "creating policy")
		pb := policyToLdapUserShort.DeepCopy()
		err = td.Kube.Create(td.Ctx, pb)
		td.Require.NoError(err, "creating policy binding")
		waitForReconcile(td, pb)

		_, err = td.Madmin.DetachPolicyLDAP(td.Ctx, madmin.PolicyAssociationReq{Policies: []string{pb.Spec.Policy}, User: pb.Spec.User.Ldap})
		td.Require.NoError(err, "detach ldap policy")
		RunOperatorUntil(td, func() error {
			pes, err := td.Madmin.GetLDAPPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
			td.Require.NoError(err, "check if policy entities updated")
			if len(pes.PolicyMappings) == 0 {
				return nil
			}
			return StopIteration{}
		})

		pes, err := td.Madmin.GetLDAPPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy entities updated")
		u := fmt.Sprintf("cn=%s,ou=users,dc=example,dc=org", pb.Spec.User.Ldap)
		td.Require.Contains(pes.PolicyMappings[0].Users, u, "check if policy entities contain user")
	})

	t.Run("recreates minio policy binding on change", func(t *testing.T) {
		td := Setup(t)

		pb := createBuiltinUserPolicyBinding(td)
		opb := pb.DeepCopy()
		waitForReconcile(td, pb)

		u2 := builtinUser2.DeepCopy()
		err := td.Kube.Create(td.Ctx, u2)
		td.Require.NoError(err, "create second user")

		pb.Spec.User.Builtin = u2.Spec.AccessKey
		err = td.Kube.Update(td.Ctx, pb)
		td.Require.NoError(err, "update policy binding to use second policy binding spec")
		waitForReconcile(td, pb)

		pes, err := td.Madmin.GetPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.False(slices.Contains(pes.PolicyMappings[0].Users, opb.Spec.User.Builtin))
		td.Require.True(slices.Contains(pes.PolicyMappings[0].Users, pb.Spec.User.Builtin))
	})

	t.Run("ensure resource version stabilizes", func(t *testing.T) {
		td := Setup(t)

		pb := createBuiltinUserPolicyBinding(td)
		waitForReconcile(td, pb)

		WaitForStableResourceVersion(td, pb)
	})

	t.Run("can still delete a minio policy binding when builtin user deleted", func(t *testing.T) {
		td := Setup(t)

		pb := createBuiltinUserPolicyBinding(td)
		waitForReconcile(td, pb)

		u := builtinUser.DeepCopy()
		err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(u), u)
		td.Require.NoError(err, "get user object")
		err = td.Kube.Delete(td.Ctx, u)
		td.Require.NoError(err, "delete user object")
		WaitForDelete(td, u)

		err = td.Kube.Delete(td.Ctx, pb)
		td.Require.NoError(err, "delete policy binding object")
		WaitForDelete(td, pb)

		pes, err := td.Madmin.GetPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.Len(pes.PolicyMappings, 0, "policy has no bindings")
	})

	t.Run("can still delete minio policy binding when builtin group deleted", func(t *testing.T) {
		td := Setup(t)

		pb := createBuiltinGroupPolicyBinding(td)
		waitForReconcile(td, pb)

		g := builtinGroup.DeepCopy()
		err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(g), g)
		td.Require.NoError(err, "get group object")
		err = td.Kube.Delete(td.Ctx, g)
		td.Require.NoError(err, "delete group object")
		WaitForDelete(td, g)

		err = td.Kube.Delete(td.Ctx, pb)
		td.Require.NoError(err, "delete policy binding object")
		WaitForDelete(td, pb)

		pes, err := td.Madmin.GetPolicyEntities(td.Ctx, madmin.PolicyEntitiesQuery{Policy: []string{pb.Spec.Policy}})
		td.Require.NoError(err, "check if policy binding exists")
		td.Require.Len(pes.PolicyMappings, 0, "policy has no bindings")
	})
}

func TestMinioUser(t *testing.T) {
	createUser := func(td TestData) *v1.MinioUser {
		td.T.Helper()

		us := builtinUserSecret.DeepCopy()
		err := td.Kube.Create(td.Ctx, us)
		td.Require.NoError(err, "create user secret object")
		u := builtinUser.DeepCopy()
		err = td.Kube.Create(td.Ctx, u)
		td.Require.NoError(err, "create user object")

		return u
	}

	waitForReconcile := func(td TestData, u *v1.MinioUser) {
		RunOperatorUntil(td, func() error {
			err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(u), u)
			td.Require.NoError(err, "waiting for user reconcile")
			if u.Status.CurrentSpec == nil {
				return nil
			}
			if !reflect.DeepEqual(u.Spec, *u.Status.CurrentSpec) {
				return nil
			}
			return StopIteration{}
		})
	}

	t.Run("creates a minio user", func(t *testing.T) {
		td := Setup(t)

		u := createUser(td)
		waitForReconcile(td, u)

		_, err := td.Madmin.GetUserInfo(td.Ctx, u.Spec.AccessKey)
		td.Require.NoError(err, "check if user exists")

		c, err := madmin.New("minio.default.svc", u.Spec.AccessKey, builtinUserSecret.StringData["SecretKey"], true)
		c.SetCustomTransport(&http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}})
		td.Require.NoError(err, "create client with user credentials")
		_, err = c.AccountInfo(td.Ctx, madmin.AccountOpts{})
		td.Require.NoError(err, "check if user credentials valid")
	})

	t.Run("does not create a minio group if one exists", func(t *testing.T) {
		td := Setup(t)

		u := createUser(td)
		sk := builtinUserSecret.StringData["SecretKey"]
		err := td.Madmin.AddUser(td.Ctx, u.Spec.AccessKey, sk)
		td.Require.NoError(err, "create minio user")

		WaitForReconcilerError(td, func(err error) error {
			if err.Error() != fmt.Sprintf("user %s already exists", u.Spec.AccessKey) {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("migrates an existing group if migrate set to true", func(t *testing.T) {
		td := Setup(t)

		u := createUser(td)
		sk := builtinUserSecret.StringData["SecretKey"]
		err := td.Madmin.AddUser(td.Ctx, u.Spec.AccessKey, string(sk))
		td.Require.NoError(err, "create minio user")
		u.Spec.Migrate = true
		err = td.Kube.Update(td.Ctx, u)
		td.Require.NoError(err, "update user resource")

		waitForReconcile(td, u)
		td.Require.False(u.Spec.Migrate, "migrate unset on reconcile")
	})

	t.Run("deletes a minio user", func(t *testing.T) {
		td := Setup(t)

		u := createUser(td)
		waitForReconcile(td, u)

		err := td.Kube.Delete(td.Ctx, u)
		td.Require.NoError(err, "delete user object")
		WaitForDelete(td, u)

		_, err = td.Madmin.GetUserInfo(td.Ctx, u.Spec.AccessKey)
		merr := &madmin.ErrorResponse{}
		td.Require.ErrorAs(err, merr, "check expected error type")
		td.Require.Equal(merr.Code, "XMinioAdminNoSuchUser", "check expected error code")
	})

	t.Run("recreates minio user if deleted in background", func(t *testing.T) {
		td := Setup(t)

		u := createUser(td)
		waitForReconcile(td, u)

		err := td.Madmin.RemoveUser(td.Ctx, u.Spec.AccessKey)
		td.Require.NoError(err, "delete minio user resource")
		RunOperatorUntil(td, func() error {
			_, err := td.Madmin.GetUserInfo(td.Ctx, u.Spec.AccessKey)
			if merr, ok := err.(madmin.ErrorResponse); ok && merr.Code == "XMinioAdminNoSuchUser" {
				return nil
			}
			td.Require.NoError(err, "check if user created")
			return StopIteration{}
		})
	})

	t.Run("deletes resource even when minio user missing", func(t *testing.T) {
		td := Setup(t)

		u := createUser(td)
		waitForReconcile(td, u)

		err := td.Madmin.RemoveUser(td.Ctx, u.Spec.AccessKey)
		td.Require.NoError(err, "delete minio user resource")

		err = td.Kube.Delete(td.Ctx, u)
		td.Require.NoError(err, "delete user object")
		WaitForDelete(td, u)
	})

	t.Run("updates minio user when secret key ref changes", func(t *testing.T) {
		td := Setup(t)

		u := createUser(td)
		waitForReconcile(td, u)

		us := builtinUserSecret.DeepCopy()
		err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(us), us)
		td.Require.NoError(err, "fetch user secret")

		us.Data["SecretKey"] = []byte("password2")
		err = td.Kube.Update(td.Ctx, us)
		td.Require.NoError(err, "update user secret")

		RunOperatorUntil(td, func() error {
			err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(u), u)
			td.Require.NoError(err, "check if user secret key updated")
			if u.Status.CurrentSecretKeyRefResourceVersion != us.GetResourceVersion() {
				return nil
			}
			return StopIteration{}
		})

		c, err := madmin.New("minio.default.svc", u.Spec.AccessKey, string(us.Data["SecretKey"]), true)
		c.SetCustomTransport(&http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}})
		td.Require.NoError(err, "create client with new user credentials")
		_, err = c.AccountInfo(td.Ctx, madmin.AccountOpts{})
		td.Require.NoError(err, "check if user credentials valid")
	})

	t.Run("ensure resource version stabilizes", func(t *testing.T) {
		td := Setup(t)

		u := createUser(td)
		waitForReconcile(td, u)

		WaitForStableResourceVersion(td, u)
	})
}

func TestMinioAccessKey(t *testing.T) {
	createBuiltinAccessKey := func(td TestData) *v1.MinioAccessKey {
		td.T.Helper()

		us := builtinUserSecret.DeepCopy()
		err := td.Kube.Create(td.Ctx, us)
		td.Require.NoError(err, "create user secret object")
		u := builtinUser.DeepCopy()
		err = td.Kube.Create(td.Ctx, u)
		td.Require.NoError(err, "create user object")

		ak := builtinAccessKey.DeepCopy()
		err = td.Kube.Create(td.Ctx, ak)
		td.Require.NoError(err, "create builtin access key")

		return ak
	}

	createLdapAccessKey := func(td TestData) *v1.MinioAccessKey {
		td.T.Helper()

		// NOTE: it appears for LDAP, a policy needs to be bound to the user
		p := policy.DeepCopy()
		err := td.Kube.Create(td.Ctx, p)
		td.Require.NoError(err, "create user policy")
		pb := policyToLdapUserShort.DeepCopy()
		err = td.Kube.Create(td.Ctx, pb)
		td.Require.NoError(err, "create user policy binding")

		ak := ldapAccessKey.DeepCopy()
		err = td.Kube.Create(td.Ctx, ak)
		td.Require.NoError(err, "create ldap access key")

		return ak
	}

	waitForReconcile := func(td TestData, ak *v1.MinioAccessKey) {
		RunOperatorUntil(td, func() error {
			err := td.Kube.Get(td.Ctx, client.ObjectKeyFromObject(ak), ak)
			td.Require.NoError(err, "waiting for access key reconcile")
			if ak.Status.CurrentSpec == nil {
				return nil
			}
			if !reflect.DeepEqual(ak.Spec, *ak.Status.CurrentSpec) {
				return nil
			}

			return StopIteration{}
		})
	}

	getGeneratedSecret := func(td TestData, ak *v1.MinioAccessKey) *corev1.Secret {
		td.T.Helper()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: ak.Status.CurrentSpec.SecretName},
		}
		err := td.Kube.Get(td.Ctx, types.NamespacedName{Name: ak.Status.CurrentSpec.SecretName, Namespace: ak.GetNamespace()}, secret)
		td.Require.NoError(err, "check if generated secret exists")

		return secret
	}

	t.Run("creates a builtin minio access key", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])
		td.Require.NotEmpty(accessKey, "check if generated secret accessKey is set")
		td.Require.NotEmpty(secret.Data[operator.AccessKeyFieldSecretKey], "check if generated secret secretKey is set")

		_, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check minio access key exists")
	})

	t.Run("creates an ldap minio access key", func(t *testing.T) {
		td := Setup(t)
		SetMinioLDAPIdentityProvider(td)

		ak := createLdapAccessKey(td)
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])
		td.Require.NotEmpty(accessKey, "check if generated secret accessKey is set")
		td.Require.NotEmpty(secret.Data[operator.AccessKeyFieldSecretKey], "check if generated secret secretKey is set")

		_, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check if minio access key exists")
	})

	t.Run("creates a minio access key with an explicit access key", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		ak.Spec.AccessKey = "TESTING"
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")

		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])
		td.Require.Equal(accessKey, ak.Spec.AccessKey, "check generated secret access key matches")

		_, err = td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check if minio access key exists")
	})

	t.Run("creates a minio access key with an explicit secret key", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		ak.Spec.SecretKeyRef = v1.ResourceKeyRef{Name: builtinUserSecret.Name, Key: "SecretKey"}
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")

		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		secretKey := string(secret.Data[operator.AccessKeyFieldSecretKey])
		td.Require.Equal(secretKey, builtinUserSecret.StringData["SecretKey"], "check generated secret key matches")
	})

	t.Run("creates a minio access key with a policy", func(t *testing.T) {
		td := Setup(t)

		policy := v1.MinioAccessKeyPolicy{
			Statement: policy.Spec.Statement,
			Version:   policy.Spec.Version,
		}
		ak := createBuiltinAccessKey(td)
		ak.Spec.Policy = policy
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")

		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		mak, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check if minio access key exists")
		var mp v1.MinioAccessKeyPolicy
		err = json.Unmarshal([]byte(mak.Policy), &mp)
		td.Require.NoError(err, "unmarshal minio access key policy")
		td.Require.Equal(policy, mp, "check if policies are equal")
	})

	t.Run("creates a minio access key with a name", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		ak.Spec.Name = "test name"
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")

		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		mak, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check if minio access key exists")
		td.Require.Equal(mak.Name, ak.Spec.Name, "check if name equal")
	})

	t.Run("creates a minio access key with a description", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		ak.Spec.Description = "test description"
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")

		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		mak, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check if minio access key exists")
		td.Require.Equal(mak.Description, ak.Spec.Description, "check if description equal")
	})

	t.Run("creates a minio access key with an expiration", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		expiration := time.Now().UTC().Add(1 * time.Hour).Truncate(time.Second)
		ak.Spec.Expiration = &metav1.Time{Time: expiration}
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")

		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		mak, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check if minio access key exists")
		td.Require.Equal(mak.Expiration, &expiration, "check if time equal")
	})

	t.Run("updates a minio access key when expiration changes", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		waitForReconcile(td, ak)

		expiration := time.Now().UTC().Add(1 * time.Hour).Truncate(time.Second)
		ak.Spec.Expiration = &metav1.Time{Time: expiration}
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		mak, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check if minio access key exists")
		td.Require.Equal(mak.Expiration, &expiration, "check if time equal")
	})

	t.Run("updates a minio access key when description changes", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		waitForReconcile(td, ak)

		ak.Spec.Description = "test description"
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		mak, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check if minio access key exists")
		td.Require.Equal(mak.Description, ak.Spec.Description, "check if description equal")
	})

	t.Run("updates a minio access key when name changes", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		waitForReconcile(td, ak)

		ak.Spec.Name = "test name"
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		mak, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check if minio access key exists")
		td.Require.Equal(mak.Name, ak.Spec.Name, "check if name equal")
	})

	t.Run("updates a minio access key when policy changes", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		waitForReconcile(td, ak)

		policy := v1.MinioAccessKeyPolicy{
			Statement: policy.Spec.Statement,
			Version:   policy.Spec.Version,
		}
		ak.Spec.Policy = policy
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		mak, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		td.Require.NoError(err, "check if minio access key exists")
		var mp v1.MinioAccessKeyPolicy
		err = json.Unmarshal([]byte(mak.Policy), &mp)
		td.Require.NoError(err, "unmarshal minio access key policy")
		td.Require.Equal(policy, mp, "check if policies are equal")
	})

	t.Run("updates a minio access key when secret key changes", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		waitForReconcile(td, ak)

		ak.Spec.SecretKeyRef = v1.ResourceKeyRef{Name: builtinUserSecret.Name, Key: "SecretKey"}
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		secretKey := string(secret.Data[operator.AccessKeyFieldSecretKey])
		td.Require.Equal(secretKey, builtinUserSecret.StringData["SecretKey"], "check generated secret key matches")
	})

	t.Run("resyncs access key when remote has different name", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		ak.Spec.Name = "testing"
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		err = td.Madmin.UpdateServiceAccount(td.Ctx, accessKey, madmin.UpdateServiceAccountReq{NewName: "different"})
		td.Require.NoError(err, "update minio access key")

		RunOperatorUntil(td, func() error {
			mi, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
			td.Require.NoError(err, "fetch minio service account")
			if mi.Name != ak.Spec.Name {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("resyncs access key when remote has different description", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		ak.Spec.Description = "testing"
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		err = td.Madmin.UpdateServiceAccount(td.Ctx, accessKey, madmin.UpdateServiceAccountReq{NewDescription: "different"})
		td.Require.NoError(err, "update minio access key")

		RunOperatorUntil(td, func() error {
			mi, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
			td.Require.NoError(err, "fetch minio service account")
			if mi.Description != ak.Spec.Description {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("resyncs access key when remote has different expiration", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		expiration := time.Now().UTC().Add(1 * time.Hour).Truncate(time.Second)
		ak.Spec.Expiration = &metav1.Time{Time: expiration}
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		err = td.Madmin.UpdateServiceAccount(td.Ctx, accessKey, madmin.UpdateServiceAccountReq{NewExpiration: nil})
		td.Require.NoError(err, "update minio access key")

		RunOperatorUntil(td, func() error {
			mi, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
			td.Require.NoError(err, "fetch minio service account")
			if !reflect.DeepEqual(mi.Expiration, &expiration) {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("resyncs access key when remote has different policy", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		policy := v1.MinioAccessKeyPolicy{
			Statement: policy.Spec.Statement,
			Version:   policy.Spec.Version,
		}
		ak.Spec.Policy = policy
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		err = td.Madmin.UpdateServiceAccount(td.Ctx, accessKey, madmin.UpdateServiceAccountReq{NewPolicy: nil})
		td.Require.NoError(err, "update minio access key")

		RunOperatorUntil(td, func() error {
			mi, err := td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
			td.Require.NoError(err, "fetch minio service account")
			mp := v1.MinioAccessKeyPolicy{}
			err = json.Unmarshal([]byte(mi.Policy), &mp)
			td.Require.NoError(err, "parse minio access key policy")
			if err != nil {
				return nil
			}
			if !reflect.DeepEqual(policy, mp) {
				return nil
			}
			return StopIteration{}
		})
	})

	t.Run("regenerates generated secret when deleted", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		err := td.Kube.Delete(td.Ctx, secret)
		td.Require.NoError(err, "delete generated secret")

		RunOperatorUntil(td, func() error {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: ak.Status.CurrentSpec.SecretName},
			}
			err := td.Kube.Get(td.Ctx, types.NamespacedName{Name: ak.Status.CurrentSpec.SecretName, Namespace: ak.GetNamespace()}, secret)
			if err != nil {
				return nil
			}
			return StopIteration{}
		})

		getGeneratedSecret(td, ak)
	})

	t.Run("regenerates generated secret when using a new name", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		oldSecretName := ak.Spec.SecretName
		waitForReconcile(td, ak)

		ak.Spec.SecretName = accessKeySecret2.GetName()
		err := td.Kube.Update(td.Ctx, ak)
		td.Require.NoError(err, "update access key resource")
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		td.Require.Equal(ak.Spec.SecretName, secret.GetName(), "check if generated secret name matches")

		err = td.Kube.Get(td.Ctx, types.NamespacedName{Namespace: ak.GetNamespace(), Name: oldSecretName}, &corev1.Secret{})
		td.Require.True(errors.IsNotFound(err), "check if old generated secret deleted")
	})

	t.Run("deletes a minio access key", func(t *testing.T) {
		td := Setup(t)

		ak := createBuiltinAccessKey(td)
		waitForReconcile(td, ak)

		secret := getGeneratedSecret(td, ak)
		accessKey := string(secret.Data[operator.AccessKeyFieldAccessKey])

		err := td.Kube.Delete(td.Ctx, ak)
		td.Require.NoError(err, "delete access key object")
		WaitForDelete(td, ak)

		_, err = td.Madmin.InfoServiceAccount(td.Ctx, accessKey)
		merr := &madmin.ErrorResponse{}
		td.Require.ErrorAs(err, merr, "check expected error type")
		td.Require.Equal(merr.Code, "XMinioInvalidIAMCredentials", "check expected error code")

		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: ak.Status.CurrentSpec.SecretName},
		}
		err = td.Kube.Get(td.Ctx, types.NamespacedName{Name: ak.Status.CurrentSpec.SecretName, Namespace: ak.GetNamespace()}, secret)
		td.Require.Error(err, "check if secret is removed")
	})

	t.Run("ensure resource version stabilizes", func(t *testing.T) {
		td := Setup(t)

		b := createBuiltinAccessKey(td)
		waitForReconcile(td, b)

		WaitForStableResourceVersion(td, b)
	})
}

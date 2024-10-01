package operator

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"

	v1 "github.com/benfiola/minio-operator-ext/pkg/api/bfiola.dev/v1"
	"github.com/go-logr/logr"
	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	miniov2 "github.com/minio/operator/pkg/apis/minio.min.io/v2"
	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var doNotDeleteFinalizer = "bfiola.dev/prevent-deletion-for-testing"

type TestData struct {
	Bucket                    v1.MinioBucket
	BuiltinGroup              v1.MinioGroup
	BuiltinGroupToPolicy      v1.MinioPolicyBinding
	BuiltinUser               v1.MinioUser
	BuiltinUserSecret         corev1.Secret
	BuiltinUserToBuiltinGroup v1.MinioGroupBinding
	BuiltinUserToPolicy       v1.MinioPolicyBinding
	LdapGroupToPolicy         v1.MinioPolicyBinding
	LdapUserToPolicy          v1.MinioPolicyBinding
	Policy                    v1.MinioPolicy
}

type OperatorTestSuite struct {
	suite.Suite
	Kubeconfig string
	Kube       client.Client
	Ctx        context.Context
	Minio      *minio.Client
	Madmin     *madmin.AdminClient
	TestData   *TestData
}

// Creates a [reconcile.Request] from a [client.Object]
func (s *OperatorTestSuite) GetReconcileRequest(o client.Object) reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: o.GetNamespace(),
			Name:      o.GetName(),
		},
	}
}

// Runs a reconciler until a hard limit is reached, the targeted resource stops changing, or an error is encountered.
// Returns the last reconciliation result and error (if any) returned by the reconciler
func (s *OperatorTestSuite) RunReconciler(r reconciler, o client.Object) (reconcile.Result, error) {
	var err error
	var rres reconcile.Result
	var curr int
	limit := 10
	rr := s.GetReconcileRequest(o)

	for curr = range limit {
		s.Require().Nil(s.Kube.Get(s.Ctx, rr.NamespacedName, o, &client.GetOptions{}))
		b := o.GetResourceVersion()
		rres, err = r.Reconcile(s.Ctx, rr)
		if err != nil {
			break
		}
		err = s.Kube.Get(s.Ctx, rr.NamespacedName, o, &client.GetOptions{})
		s.Require().Nil(err)
		a := o.GetResourceVersion()
		if b == a {
			break
		}
	}

	if curr == limit {
		err = fmt.Errorf("reconciliation limit reached")
	}

	return rres, err
}

// Returns a pointer to a string
func (s *OperatorTestSuite) Ptr(v string) *string {
	return &v
}

// Creates testing data
// These kubernetes resources are cleaned up and re-created before every test.
// It's up to test implementations to modify these resources are required to perform adequate testing.
func (s *OperatorTestSuite) CreateTestData() *TestData {
	tr := v1.ResourceRef{
		Namespace: "default",
		Name:      "myminio",
	}
	b := v1.MinioBucket{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "bucket",
		},
		Spec: v1.MinioBucketSpec{
			Name:      "bucket",
			TenantRef: tr,
		},
	}
	bg := v1.MinioGroup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "builtin-group",
		},
		Spec: v1.MinioGroupSpec{
			Name:      "builtin-group",
			TenantRef: tr,
		},
	}
	p := v1.MinioPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "policy",
		},
		Spec: v1.MinioPolicySpec{
			Name: "policy",
			Statement: []v1.MinioPolicyStatement{
				{
					Action: []string{
						"s3:AbortMultipartUpload",
						"s3:DeleteObject",
						"s3:ListMultipartUploadParts",
						"s3:PutObject",
						"s3:GetObject",
					},
					Effect: "Allow",
					Resource: []string{
						fmt.Sprintf("arn:aws:s3:::%s/*", b.Name),
					},
				},
			},
			TenantRef: tr,
			Version:   "2012-10-17",
		},
	}
	bus := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "builtin-user-secret",
		},
		StringData: map[string]string{
			"SecretKey": "password",
		},
	}
	bu := v1.MinioUser{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "builtin-user",
		},
		Spec: v1.MinioUserSpec{
			AccessKey: "builtin-user",
			SecretKeyRef: v1.ResourceKeyRef{
				Namespace: bus.Namespace,
				Name:      bus.Name,
				Key:       "SecretKey",
			},
			TenantRef: tr,
		},
	}
	butbg := v1.MinioGroupBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "builtin-user-to-builtin-group",
		},
		Spec: v1.MinioGroupBindingSpec{
			Group:     bg.Name,
			TenantRef: tr,
			User:      bu.Name,
		},
	}
	butp := v1.MinioPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "builtin-user-to-policy",
		},
		Spec: v1.MinioPolicyBindingSpec{
			Policy:    p.Name,
			TenantRef: tr,
			User: v1.MinioPolicyBindingIdentity{
				Builtin: s.Ptr(bu.Name),
			},
		},
	}
	bgtp := v1.MinioPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "builtin-group-to-policy",
		},
		Spec: v1.MinioPolicyBindingSpec{
			Group: v1.MinioPolicyBindingIdentity{
				Builtin: s.Ptr(bg.Name),
			},
			Policy:    p.Name,
			TenantRef: tr,
		},
	}
	lutp := v1.MinioPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "ldap-user-to-policy",
		},
		Spec: v1.MinioPolicyBindingSpec{
			Group: v1.MinioPolicyBindingIdentity{
				Ldap: s.Ptr("ldap-user1"),
			},
			Policy:    p.Name,
			TenantRef: tr,
		},
	}
	lgtp := v1.MinioPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "ldap-group-to-policy",
		},
		Spec: v1.MinioPolicyBindingSpec{
			Group: v1.MinioPolicyBindingIdentity{
				Ldap: s.Ptr("ldap-group1"),
			},
			Policy:    p.Name,
			TenantRef: tr,
		},
	}
	return &TestData{
		Bucket:                    b,
		BuiltinGroup:              bg,
		BuiltinGroupToPolicy:      bgtp,
		BuiltinUser:               bu,
		BuiltinUserSecret:         bus,
		BuiltinUserToBuiltinGroup: butbg,
		BuiltinUserToPolicy:       butp,
		LdapGroupToPolicy:         lgtp,
		LdapUserToPolicy:          lutp,
		Policy:                    p,
	}
}

// Setup the [OperatorTestSuite] for testing
func (s *OperatorTestSuite) SetupSuite() {
	s.T().Logf("creating cluster")
	pd := filepath.Join("..", "..")
	cmd := exec.Command("make", "create-cluster")
	cmd.Dir = pd
	output, err := cmd.CombinedOutput()
	if err != nil {
		s.Require().Nil(fmt.Errorf("make create-cluster failed: %s", output))
	}

	s.Kubeconfig = filepath.Join(pd, ".dev", "kube-config.yaml")

	s.T().Logf("creating client")
	rc, err := clientcmd.BuildConfigFromFlags("", s.Kubeconfig)
	s.Require().Nil(err)
	rs := runtime.NewScheme()
	s.Require().Nil(v1.AddToScheme(rs))
	s.Require().Nil(kscheme.AddToScheme(rs))
	s.Require().Nil(miniov2.AddToScheme(rs))
	s.Kube, err = client.New(rc, client.Options{
		Scheme: rs,
	})
	s.Require().Nil(err)

	me := "minio.default.svc"
	mak := "minio"
	msk := "minio123"

	s.T().Logf("creating minio client")
	s.Minio, err = minio.New(me, &minio.Options{
		Creds: credentials.NewStaticV4(mak, msk, ""),
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
		Secure: true,
	})
	s.Require().Nil(err)

	s.T().Logf("creating minio admin client")
	s.Madmin, err = madmin.New(me, mak, msk, true)
	s.Madmin.SetCustomTransport(&http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	})
	s.Require().Nil(err)
}

// Setup each sub test within an [OperatorTestSuite]
func (s *OperatorTestSuite) SetupSubTest() {
	s.T().Logf("creating context")
	s.Ctx = context.Background()

	// helper function to remove custom resources
	remove := func(o client.Object) {
		nsn := types.NamespacedName(types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()})
		err := s.Kube.Get(s.Ctx, nsn, o)
		if err != nil && apierrors.IsNotFound(err) {
			return
		}
		s.T().Logf("removing %s", nsn.String())
		s.Require().Nil(err)
		// remove finalizers
		controllerutil.RemoveFinalizer(o, finalizer)
		controllerutil.RemoveFinalizer(o, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, o))
		// only attempt deletion if object isn't already being deleted
		if o.GetDeletionTimestamp().IsZero() {
			s.Require().Nil(s.Kube.Delete(s.Ctx, o))
		}
	}

	if s.TestData == nil {
		// NOTE: this is needed for cleanup on first-run
		s.TestData = s.CreateTestData()
	}
	s.T().Log("removing resources")
	remove(&s.TestData.Bucket)
	remove(&s.TestData.BuiltinGroup)
	remove(&s.TestData.BuiltinGroupToPolicy)
	remove(&s.TestData.BuiltinUser)
	remove(&s.TestData.BuiltinUserSecret)
	remove(&s.TestData.BuiltinUserToBuiltinGroup)
	remove(&s.TestData.BuiltinUserToPolicy)
	remove(&s.TestData.LdapGroupToPolicy)
	remove(&s.TestData.LdapUserToPolicy)
	remove(&s.TestData.Policy)

	s.T().Log("removing minio buckets")
	bl := []string{s.TestData.Bucket.Spec.Name}
	for _, b := range bl {
		s.T().Logf("removing %s", b)
		err := s.Minio.RemoveBucket(s.Ctx, b)
		if err != nil {
			s.Require().ErrorContains(err, "bucket does not exist")
		}
	}
	s.T().Log("removing minio users")
	ul := []string{s.TestData.BuiltinUser.Spec.AccessKey}
	for _, u := range ul {
		s.T().Logf("removing %s", u)
		err := s.Madmin.RemoveUser(s.Ctx, u)
		if err != nil {
			s.Require().ErrorContains(err, "user does not exist")
		}
	}
	gl := []string{s.TestData.BuiltinGroup.Spec.Name}
	for _, g := range gl {
		s.T().Logf("removing %s", g)
		err := s.Madmin.UpdateGroupMembers(s.Ctx, madmin.GroupAddRemove{Group: g, IsRemove: true})
		if err != nil {
			s.Require().ErrorContains(err, "group does not exist")
		}
	}
	s.T().Log("removing minio policies")
	pl := []string{s.TestData.Policy.Spec.Name}
	for _, p := range pl {
		s.Require().Nil(s.Madmin.RemoveCannedPolicy(s.Ctx, p))
	}

	s.T().Logf("creating resources")
	create := func(o client.Object) {
		s.Require().Nil(s.Kube.Create(s.Ctx, o))
	}
	s.TestData = s.CreateTestData()
	create(&s.TestData.Bucket)
	create(&s.TestData.BuiltinGroup)
	create(&s.TestData.BuiltinGroupToPolicy)
	create(&s.TestData.BuiltinUser)
	create(&s.TestData.BuiltinUserSecret)
	create(&s.TestData.BuiltinUserToBuiltinGroup)
	create(&s.TestData.BuiltinUserToPolicy)
	create(&s.TestData.LdapGroupToPolicy)
	create(&s.TestData.LdapUserToPolicy)
	create(&s.TestData.Policy)
}

func TestOperatorTestSuite(t *testing.T) {
	suite.Run(t, new(OperatorTestSuite))
}

func (s *OperatorTestSuite) TestMinioBucketReconciler() {
	createReconciler := func() minioBucketReconciler {
		return minioBucketReconciler{
			Client: s.Kube,
			logger: logr.Discard(),
		}
	}

	s.Run("add finalizer when unset", func() {
		b := &s.TestData.Bucket
		s.Require().False(controllerutil.ContainsFinalizer(b, finalizer))
		s.Require().Nil(s.Kube.Update(s.Ctx, b))
		r := createReconciler()
		_, err := s.RunReconciler(&r, b)
		s.Require().Nil(err)
		s.Require().True(controllerutil.ContainsFinalizer(b, finalizer))
	})

	s.Run("create resource succeeds", func() {
		b := &s.TestData.Bucket
		controllerutil.AddFinalizer(b, finalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, b))
		r := createReconciler()
		_, err := s.RunReconciler(&r, b)
		s.Require().Nil(err)
		mb, err := s.Minio.BucketExists(s.Ctx, b.Spec.Name)
		s.Require().Nil(err)
		s.Require().True(mb)
	})

	s.Run("create resource fails if minio bucket already exists", func() {
		b := &s.TestData.Bucket
		controllerutil.AddFinalizer(b, finalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, b))
		s.Require().Nil(s.Minio.MakeBucket(s.Ctx, b.Spec.Name, minio.MakeBucketOptions{}))
		r := createReconciler()
		_, err := s.RunReconciler(&r, b)
		errr := &minio.ErrorResponse{}
		s.Require().ErrorAs(err, errr)
		s.Require().Equal(errr.Code, "BucketAlreadyOwnedByYou")
		s.Require().Nil(b.Status.CurrentSpec)
	})

	s.Run("delete resource succeeds", func() {
		b := &s.TestData.Bucket
		b.Status.CurrentSpec = &b.Spec
		s.Require().Nil(s.Minio.MakeBucket(s.Ctx, b.Spec.Name, minio.MakeBucketOptions{}))
		controllerutil.AddFinalizer(b, finalizer)
		controllerutil.AddFinalizer(b, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, b))
		s.Require().Nil(s.Kube.Delete(s.Ctx, b))
		r := createReconciler()
		_, err := s.RunReconciler(&r, b)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(b, finalizer))
		s.Require().False(s.Minio.BucketExists(s.Ctx, b.Spec.Name))
	})

	s.Run("delete resource succeeds when not reconciled", func() {
		b := &s.TestData.Bucket
		controllerutil.AddFinalizer(b, finalizer)
		controllerutil.AddFinalizer(b, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, b))
		s.Require().Nil(s.Kube.Delete(s.Ctx, b))
		r := createReconciler()
		_, err := s.RunReconciler(&r, b)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(b, finalizer))
	})

	s.Run("delete resource succeeds when minio bucket does not exist", func() {
		b := &s.TestData.Bucket
		b.Status.CurrentSpec = &b.Spec
		controllerutil.AddFinalizer(b, finalizer)
		controllerutil.AddFinalizer(b, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, b))
		s.Require().Nil(s.Kube.Delete(s.Ctx, b))
		r := createReconciler()
		_, err := s.RunReconciler(&r, b)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(b, finalizer))
	})
}

func (s *OperatorTestSuite) TestMinioGroupReconciler() {
	createReconciler := func() minioGroupReconciler {
		return minioGroupReconciler{
			Client: s.Kube,
			logger: logr.Discard(),
		}
	}

	s.Run("add finalizer when unset", func() {
		g := &s.TestData.BuiltinGroup
		s.Require().False(controllerutil.ContainsFinalizer(g, finalizer))
		s.Require().Nil(s.Kube.Update(s.Ctx, g))
		r := createReconciler()
		_, err := s.RunReconciler(&r, g)
		s.Require().Nil(err)
		s.Require().True(controllerutil.ContainsFinalizer(g, finalizer))
	})

	s.Run("create resource succeeds", func() {
		g := &s.TestData.BuiltinGroup
		controllerutil.AddFinalizer(g, finalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, g))
		r := createReconciler()
		_, err := s.RunReconciler(&r, g)
		s.Require().Nil(err)
		_, err = s.Madmin.GetGroupDescription(s.Ctx, g.Spec.Name)
		s.Require().Nil(err)
	})

	s.Run("create resource fails if minio group already exists", func() {
		g := &s.TestData.BuiltinGroup
		controllerutil.AddFinalizer(g, finalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, g))
		s.Require().Nil(s.Madmin.UpdateGroupMembers(s.Ctx, madmin.GroupAddRemove{Group: g.Spec.Name}))
		r := createReconciler()
		_, err := s.RunReconciler(&r, g)
		s.Require().NotNil(err)
		s.Require().Nil(g.Status.CurrentSpec)
	})

	s.Run("delete resource succeeds", func() {
		g := &s.TestData.BuiltinGroup
		g.Status.CurrentSpec = &g.Spec
		s.Require().Nil(s.Madmin.UpdateGroupMembers(s.Ctx, madmin.GroupAddRemove{Group: g.Spec.Name}))
		controllerutil.AddFinalizer(g, finalizer)
		controllerutil.AddFinalizer(g, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, g))
		s.Require().Nil(s.Kube.Delete(s.Ctx, g))
		r := createReconciler()
		_, err := s.RunReconciler(&r, g)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(g, finalizer))
		_, err = s.Madmin.GetGroupDescription(s.Ctx, g.Spec.Name)
		s.Require().NotNil(err)
	})

	s.Run("delete resource succeeds when not reconciled", func() {
		g := &s.TestData.BuiltinGroup
		controllerutil.AddFinalizer(g, finalizer)
		controllerutil.AddFinalizer(g, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, g))
		s.Require().Nil(s.Kube.Delete(s.Ctx, g))
		r := createReconciler()
		_, err := s.RunReconciler(&r, g)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(g, finalizer))
	})

	s.Run("delete resource succeeds when minio group does not exist", func() {
		g := &s.TestData.BuiltinGroup
		g.Status.CurrentSpec = &g.Spec
		controllerutil.AddFinalizer(g, finalizer)
		controllerutil.AddFinalizer(g, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, g))
		s.Require().Nil(s.Kube.Delete(s.Ctx, g))
		r := createReconciler()
		_, err := s.RunReconciler(&r, g)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(g, finalizer))
	})
}

func (s *OperatorTestSuite) TestMinioPolicyReconciler() {
	createReconciler := func() minioPolicyReconciler {
		return minioPolicyReconciler{
			Client: s.Kube,
			logger: logr.Discard(),
		}
	}

	toJson := func(p *v1.MinioPolicy) []byte {
		pd, err := json.Marshal(map[string]any{
			"statement": p.Spec.Statement,
			"version":   p.Spec.Version,
		})
		s.Require().Nil(err)
		return pd
	}

	s.Run("add finalizer when unset", func() {
		p := &s.TestData.Policy
		s.Require().False(controllerutil.ContainsFinalizer(p, finalizer))
		s.Require().Nil(s.Kube.Update(s.Ctx, p))
		r := createReconciler()
		_, err := s.RunReconciler(&r, p)
		s.Require().Nil(err)
		s.Require().True(controllerutil.ContainsFinalizer(p, finalizer))
	})

	s.Run("create resource succeeds", func() {
		p := &s.TestData.Policy
		controllerutil.AddFinalizer(p, finalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, p))
		r := createReconciler()
		_, err := s.RunReconciler(&r, p)
		s.Require().Nil(err)
		_, err = s.Madmin.InfoCannedPolicyV2(s.Ctx, p.Spec.Name)
		s.Require().Nil(err)
	})

	s.Run("create resource fails if minio policy already exists", func() {
		p := &s.TestData.Policy
		controllerutil.AddFinalizer(p, finalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, p))
		s.Require().Nil(s.Madmin.AddCannedPolicy(s.Ctx, p.Spec.Name, toJson(p)))
		r := createReconciler()
		_, err := s.RunReconciler(&r, p)
		s.Require().ErrorContains(err, fmt.Sprintf("policy %s already exists", p.Spec.Name))
		s.Require().Nil(p.Status.CurrentSpec)
	})

	s.Run("delete resource succeeds", func() {
		p := &s.TestData.Policy
		p.Status.CurrentSpec = &p.Spec
		s.Require().Nil(s.Madmin.AddCannedPolicy(s.Ctx, p.Spec.Name, toJson(p)))
		controllerutil.AddFinalizer(p, finalizer)
		controllerutil.AddFinalizer(p, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, p))
		s.Require().Nil(s.Kube.Delete(s.Ctx, p))
		r := createReconciler()
		_, err := s.RunReconciler(&r, p)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(p, finalizer))
		_, err = s.Madmin.InfoCannedPolicyV2(s.Ctx, p.Spec.Name)
		s.Require().NotNil(err)
	})

	s.Run("delete resource succeeds when not reconciled", func() {
		p := &s.TestData.Policy
		controllerutil.AddFinalizer(p, finalizer)
		controllerutil.AddFinalizer(p, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, p))
		s.Require().Nil(s.Kube.Delete(s.Ctx, p))
		r := createReconciler()
		_, err := s.RunReconciler(&r, p)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(p, finalizer))
	})

	s.Run("delete resource succeeds when minio policy does not exist", func() {
		p := &s.TestData.Policy
		p.Status.CurrentSpec = &p.Spec
		controllerutil.AddFinalizer(p, finalizer)
		controllerutil.AddFinalizer(p, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, p))
		s.Require().Nil(s.Kube.Delete(s.Ctx, p))
		r := createReconciler()
		_, err := s.RunReconciler(&r, p)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(p, finalizer))
	})
}

func (s *OperatorTestSuite) TestMinioUserReconciler() {
	createReconciler := func() minioUserReconciler {
		return minioUserReconciler{
			Client: s.Kube,
			logger: logr.Discard(),
		}
	}

	s.Run("add finalizer when unset", func() {
		u := &s.TestData.BuiltinUser
		s.Require().False(controllerutil.ContainsFinalizer(u, finalizer))
		s.Require().Nil(s.Kube.Update(s.Ctx, u))
		r := createReconciler()
		_, err := s.RunReconciler(&r, u)
		s.Require().Nil(err)
		s.Require().True(controllerutil.ContainsFinalizer(u, finalizer))
	})

	s.Run("create resource succeeds", func() {
		u := &s.TestData.BuiltinUser
		controllerutil.AddFinalizer(u, finalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, u))
		r := createReconciler()
		_, err := s.RunReconciler(&r, u)
		s.Require().Nil(err)
		_, err = s.Madmin.GetUserInfo(s.Ctx, u.Spec.AccessKey)
		s.Require().Nil(err)
	})

	s.Run("create resource fails if minio user already exists", func() {
		sk := string(s.TestData.BuiltinUserSecret.Data["SecretKey"])
		u := &s.TestData.BuiltinUser
		controllerutil.AddFinalizer(u, finalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, u))
		s.Require().Nil(s.Madmin.AddUser(s.Ctx, u.Spec.AccessKey, sk))
		r := createReconciler()
		_, err := s.RunReconciler(&r, u)
		s.Require().ErrorContains(err, fmt.Sprintf("user %s already exists", u.Spec.AccessKey))
		s.Require().Nil(u.Status.CurrentSpec)
	})

	s.Run("delete resource succeeds", func() {
		sk := string(s.TestData.BuiltinUserSecret.Data["SecretKey"])
		u := &s.TestData.BuiltinUser
		u.Status.CurrentSpec = &u.Spec
		s.Require().Nil(s.Madmin.AddUser(s.Ctx, u.Spec.AccessKey, sk))
		controllerutil.AddFinalizer(u, finalizer)
		controllerutil.AddFinalizer(u, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, u))
		s.Require().Nil(s.Kube.Delete(s.Ctx, u))
		r := createReconciler()
		_, err := s.RunReconciler(&r, u)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(u, finalizer))
		_, err = s.Madmin.GetUserInfo(s.Ctx, u.Spec.AccessKey)
		s.Require().NotNil(err)
	})

	s.Run("delete resource succeeds when not reconciled", func() {
		u := &s.TestData.BuiltinUser
		controllerutil.AddFinalizer(u, finalizer)
		controllerutil.AddFinalizer(u, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, u))
		s.Require().Nil(s.Kube.Delete(s.Ctx, u))
		r := createReconciler()
		_, err := s.RunReconciler(&r, u)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(u, finalizer))
	})

	s.Run("delete resource succeeds when minio user does not exist", func() {
		u := &s.TestData.BuiltinUser
		u.Status.CurrentSpec = &u.Spec
		controllerutil.AddFinalizer(u, finalizer)
		controllerutil.AddFinalizer(u, doNotDeleteFinalizer)
		s.Require().Nil(s.Kube.Update(s.Ctx, u))
		s.Require().Nil(s.Kube.Delete(s.Ctx, u))
		r := createReconciler()
		_, err := s.RunReconciler(&r, u)
		s.Require().Nil(err)
		s.Require().False(controllerutil.ContainsFinalizer(u, finalizer))
	})
}

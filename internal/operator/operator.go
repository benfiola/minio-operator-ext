package operator

import (
	"bytes"
	"context"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	v1 "github.com/benfiola/minio-operator-ext/pkg/api/bfiola.dev/v1"
	"github.com/go-logr/logr"
	"github.com/hashicorp/go-envparse"
	"github.com/minio/madmin-go/v3"
	minioclient "github.com/minio/minio-go/v7"
	miniocredentials "github.com/minio/minio-go/v7/pkg/credentials"
	miniov2 "github.com/minio/operator/pkg/apis/minio.min.io/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	finalizer = "bfiola.dev/minio-operator-ext"
)

// Returns a pointer to [v].
// This primarily exists to simplify passing pointers to literals.
func ptr[T any](v T) *T {
	return &v
}

// Returns true if the error is an [minioclient.ErrorResponse] and its code matches that of [code].
func isMinioErrorCode(err error, code string) bool {
	merr, ok := err.(minioclient.ErrorResponse)
	return ok && merr.Code == code
}

// Ignores the error if the error is an [minioclient.ErrorResponse] and its code matches that of [code].
func ignoreMinioErrorCode(err error, code string) error {
	if isMinioErrorCode(err, code) {
		return nil
	}
	return err
}

// Returns true if the error is an [madmin.ErrorResponse] and its code matches that of [code].
func isMadminErrorCode(err error, code string) bool {
	merr, ok := err.(madmin.ErrorResponse)
	return ok && merr.Code == code
}

// Ignores the error if the error is an [madmin.ErrorResponse] and its code matches that of [code].
func ignoreMadminErrorCode(err error, code string) error {
	if isMadminErrorCode(err, code) {
		return nil
	}
	return err
}

// Operator is the public interface for the operator implementation
type Operator interface {
	Health() error
	Run(ctx context.Context) error
}

// operator manages all of the crd controllers
type operator struct {
	manager manager.Manager
	logger  *slog.Logger
}

// OperatorOpts defines the options used to construct a new [operator]
type OperatorOpts struct {
	KubeConfig   string
	Logger       *slog.Logger
	SyncInterval time.Duration
}

// Creates a new operator with the provided [OperatorOpts].
func NewOperator(o *OperatorOpts) (*operator, error) {
	l := o.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	c, err := clientcmd.BuildConfigFromFlags("", o.KubeConfig)
	if err != nil {
		return nil, err
	}

	si := o.SyncInterval
	if si == 0 {
		si = 60 * time.Second
	}

	s := runtime.NewScheme()
	err = v1.AddToScheme(s)
	if err != nil {
		return nil, err
	}
	err = miniov2.AddToScheme(s)
	if err != nil {
		return nil, err
	}
	err = kscheme.AddToScheme(s)
	if err != nil {
		return nil, err
	}

	m, err := manager.New(c, manager.Options{
		Client:                 client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.ConfigMap{}, &corev1.Secret{}, &corev1.Service{}, &miniov2.Tenant{}}}},
		Controller:             config.Controller{SkipNameValidation: ptr(true)},
		HealthProbeBindAddress: ":8888",
		Logger:                 logr.FromSlogHandler(l.Handler()),
		Scheme:                 s,
	})
	if err != nil {
		return nil, err
	}
	err = m.AddHealthzCheck("healthz", healthz.Ping)
	if err != nil {
		return nil, err
	}
	err = m.AddReadyzCheck("readyz", healthz.Ping)
	if err != nil {
		return nil, err
	}

	rs := []reconciler{
		&minioBucketReconciler{syncInterval: si},
		&minioGroupReconciler{syncInterval: si},
		&minioGroupBindingReconciler{syncInterval: si},
		&minioPolicyReconciler{syncInterval: si},
		&minioPolicyBindingReconciler{syncInterval: si},
		&minioUserReconciler{syncInterval: si},
	}
	for _, r := range rs {
		err = r.register(m)
		if err != nil {
			return nil, err
		}
	}

	return &operator{
		logger:  l,
		manager: m,
	}, err
}

// Performs a health check for the given [operator],
// Returns an error if the [operator] is unhealthy.
func (o *operator) Health() error {
	return nil
}

// Starts the [operator].
// Runs until terminated or if an error is thrown.
func (o *operator) Run(ctx context.Context) error {
	o.logger.Info("starting operator")
	return o.manager.Start(ctx)
}

// minioTenantClientInfo defines the data required to instantiate a minio client from a given tenant.
type minioTenantClientInfo struct {
	AccessKey string
	CaBundle  *x509.CertPool
	Endpoint  string
	SecretKey string
	Secure    bool
}

// Returns [minioTenantClientInfo] for a [miniov2.Tenant] referenced by [v1.ResourceRef].
// Returns an error if unable to fetch tenant information.
func getMinioTenantClientInfo(ctx context.Context, c client.Client, rr v1.ResourceRef) (*minioTenantClientInfo, error) {
	if rr.Namespace == "" {
		return nil, fmt.Errorf("namespace empty")
	}

	// get tenant
	t := &miniov2.Tenant{}
	err := c.Get(ctx, types.NamespacedName{Name: rr.Name, Namespace: rr.Namespace}, t)
	if err != nil {
		return nil, err
	}

	// determine if tenant secure
	s := true
	if t.Spec.RequestAutoCert != nil {
		s = *t.Spec.RequestAutoCert
	}

	// obtain access + secret key
	ts := &corev1.Secret{}
	err = c.Get(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: t.Spec.Configuration.Name}, ts)
	if err != nil {
		return nil, err
	}
	tsk := "config.env"
	tceb, ok := ts.Data[tsk]
	if !ok {
		return nil, fmt.Errorf("key %s for %s/%s not found", tsk, ts.Namespace, ts.Name)
	}
	tce, err := envparse.Parse(bytes.NewReader(tceb))
	if err != nil {
		return nil, err
	}
	k := "MINIO_ROOT_USER"
	ak, ok := tce[k]
	if !ok {
		return nil, fmt.Errorf("key %s in %s/%s/%s not found", k, ts.Namespace, ts.Name, tsk)
	}
	k = "MINIO_ROOT_PASSWORD"
	sk, ok := tce[k]
	if !ok {
		return nil, fmt.Errorf("key %s in %s/%s/%s not found", k, ts.Namespace, ts.Name, tsk)
	}

	// obtain endpoint
	tsvcn := "minio"
	tsvc := &corev1.Service{}
	err = c.Get(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: tsvcn}, tsvc)
	if err != nil {
		return nil, err
	}
	tsvcpn := "http-minio"
	if s {
		tsvcpn = "https-minio"
	}
	e := ""
	for _, p := range tsvc.Spec.Ports {
		if p.Name != tsvcpn {
			continue
		}
		e = fmt.Sprintf("%s.%s.svc:%d", tsvcn, rr.Namespace, p.Port)
		break
	}
	if e == "" {
		return nil, fmt.Errorf("endpoint for minio service %s/%s not found", tsvc.Namespace, tsvc.Name)
	}

	// obtain ca bundle
	krccm := &corev1.ConfigMap{}
	err = c.Get(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: "kube-root-ca.crt"}, krccm)
	if err != nil {
		return nil, err
	}
	cb, ok := krccm.Data["ca.crt"]
	if !ok {
		return nil, fmt.Errorf("kube root ca for %s not found", rr.Namespace)
	}

	// create cert pool with ca bundle
	cbp := x509.NewCertPool()
	cbb, _ := pem.Decode([]byte(cb))
	cbc, err := x509.ParseCertificate(cbb.Bytes)
	if err != nil {
		return nil, err
	}
	cbp.AddCert(cbc)

	return &minioTenantClientInfo{
		AccessKey: ak,
		CaBundle:  cbp,
		Endpoint:  e,
		Secure:    s,
		SecretKey: sk,
	}, nil
}

// Generates a [minioclient.Client] for the given [minioTenantClientInfo]
// Returns an error if the client cannot be created
func (mtci *minioTenantClientInfo) GetClient(ctx context.Context) (*minioclient.Client, error) {
	mtc, err := minioclient.New(mtci.Endpoint, &minioclient.Options{
		Creds:  miniocredentials.NewStaticV4(mtci.AccessKey, mtci.SecretKey, ""),
		Secure: mtci.Secure,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: mtci.CaBundle,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	return mtc, nil
}

// Generates a [madmin.AdminClient] for the given [minioTenantClientInfo]
// Returns an error if the client cannot be created
func (mtci *minioTenantClientInfo) GetAdminClient(ctx context.Context) (*madmin.AdminClient, error) {
	mtac, err := madmin.New(mtci.Endpoint, mtci.AccessKey, mtci.SecretKey, mtci.Secure)
	mtac.SetCustomTransport(&http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: mtci.CaBundle,
		},
	})
	if err != nil {
		return nil, err
	}

	return mtac, nil
}

// reconciler is the common interface implemented by all CRD reconcilers in this package
type reconciler interface {
	reconcile.Reconciler
	register(m manager.Manager) error
}

// minioBucketReconciler reconciles [v1.MinioBucket] resources
type minioBucketReconciler struct {
	client.Client
	logger       logr.Logger
	syncInterval time.Duration
}

// Builds a controller with a [minioBucketReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioBucketReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioBucket{}).Build(r)
	if err != nil {
		return err
	}
	r.logger = ctrl.GetLogger()
	return nil
}

// Reconciles a [reconcile.Request] associated with a [v1.MinioBucket].
// Returns a error if reconciliation fails.
func (r *minioBucketReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	b := &v1.MinioBucket{}
	err := r.Get(ctx, req.NamespacedName, b)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	success := func() (reconcile.Result, error) { return reconcile.Result{RequeueAfter: r.syncInterval}, nil }
	failure := func(err error) (reconcile.Result, error) { return reconcile.Result{}, err }

	if !b.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")
		if b.Status.CurrentSpec != nil {
			l.Info("delete bucket (status set)")

			l.Info("get tenant client")
			tr := b.Status.CurrentSpec.TenantRef.SetDefaultNamespace(b.GetNamespace())
			mtci, err := getMinioTenantClientInfo(ctx, r, tr)
			if err != nil {
				return failure(err)
			}
			mtc, err := mtci.GetClient(ctx)
			if err != nil {
				return failure(err)
			}

			l.Info("delete minio bucket")
			err = mtc.RemoveBucket(ctx, b.Status.CurrentSpec.Name)
			err = ignoreMinioErrorCode(err, "NoSuchBucket")
			if err != nil {
				return failure(err)
			}

			l.Info("clear status")
			b.Status.CurrentSpec = nil
			err = r.Update(ctx, b)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(b, finalizer)
		err = r.Update(ctx, b)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if !controllerutil.ContainsFinalizer(b, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(b, finalizer)
		err = r.Update(ctx, b)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if b.Status.CurrentSpec != nil {
		l.Info("check for bucket change")

		l.Info("get tenant client")
		tr := b.Status.CurrentSpec.TenantRef.SetDefaultNamespace(b.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
		if err != nil {
			return failure(err)
		}
		mtc, err := mtci.GetClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("check if minio bucket exists")
		be, err := mtc.BucketExists(ctx, b.Status.CurrentSpec.Name)
		if err != nil {
			return failure(err)
		}
		if !be {
			l.Info("clear status (minio bucket no longer exists)")
			b.Status.CurrentSpec = nil
			err := r.Update(ctx, b)
			if err != nil {
				return failure(err)
			}

			return success()
		}
	}

	if b.Status.CurrentSpec == nil {
		l.Info("create bucket (status unset)")

		l.Info("get tenant client")
		tr := b.Spec.TenantRef.SetDefaultNamespace(b.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
		if err != nil {
			return failure(err)
		}
		mtc, err := mtci.GetClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("create minio bucket")
		err = mtc.MakeBucket(ctx, b.Spec.Name, minioclient.MakeBucketOptions{})
		if err != nil {
			return failure(err)
		}

		l.Info("set status")
		b.Status.CurrentSpec = &b.Spec
		err = r.Update(ctx, b)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	return success()
}

// minioGroupReconciler reconciles [v1.MinioGroup] resources
type minioGroupReconciler struct {
	client.Client
	logger       logr.Logger
	syncInterval time.Duration
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
			mtci, err := getMinioTenantClientInfo(ctx, r, tr)
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

		l.Info("get tenant admin client")
		tr := g.Status.CurrentSpec.TenantRef.SetDefaultNamespace(g.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("check if minio group exists")
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
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("check if minio group exists")
		_, err = mtac.GetGroupDescription(ctx, g.Spec.Name)
		if err == nil {
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
		g.Status.CurrentSpec = &g.Spec
		err = r.Update(ctx, g)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	return success()
}

// minioGroupBindingReconciler reconciles [v1.MinioGroupBinding] resources
type minioGroupBindingReconciler struct {
	client.Client
	logger       logr.Logger
	syncInterval time.Duration
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
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
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
		tr := gb.Status.CurrentSpec.TenantRef.SetDefaultNamespace(gb.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

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
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
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
		gb.Status.CurrentSpec = &gb.Spec
		err = r.Update(ctx, gb)
		if err != nil {
			return failure(err)
		}

		return success()

	}

	return success()
}

// minioPolicyReconciler reconciles [v1.MinioPolicy] resources
type minioPolicyReconciler struct {
	client.Client
	logger       logr.Logger
	syncInterval time.Duration
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
			mtci, err := getMinioTenantClientInfo(ctx, r, tr)
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

		l.Info("get tenant admin client")
		tr := p.Status.CurrentSpec.TenantRef.SetDefaultNamespace(p.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
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

		l.Info("unmarshal minio policy")
		mps := &v1.MinioPolicySpec{}
		err = json.Unmarshal(mp.Policy, mps)
		if err != nil {
			return failure(err)
		}

		if !reflect.DeepEqual(*p.Status.CurrentSpec, p.Spec) {
			l.Info("update policy (status and spec differ)")

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
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
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
		p.Status.CurrentSpec = &p.Spec
		err = r.Update(ctx, p)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	return success()
}

// minioPolicyBindingReconciler reconciles [v1.MinioPolicyBinding] resources
type minioPolicyBindingReconciler struct {
	client.Client
	logger       logr.Logger
	syncInterval time.Duration
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
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
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

		l.Info("get tenant admin client")
		tr := pb.Status.CurrentSpec.TenantRef.SetDefaultNamespace(pb.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
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
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
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
		if err != nil {
			return failure(err)
		}

		l.Info("set status")
		pb.Status.CurrentSpec = &pb.Spec
		err = r.Update(ctx, pb)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	return success()
}

// minioUserReconciler reconciles [v1.MinioUser] resources
type minioUserReconciler struct {
	client.Client
	logger       logr.Logger
	syncInterval time.Duration
}

// Builds a controller with a [minioUserReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioUserReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioUser{}).Build(r)
	if err != nil {
		return err
	}
	r.logger = ctrl.GetLogger()
	return nil
}

// Reconciles a [reconcile.Request] associated with a [v1.MinioUser].
// Returns a error if reconciliation fails.
func (r *minioUserReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	u := &v1.MinioUser{}
	err := r.Get(ctx, req.NamespacedName, u)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	success := func() (reconcile.Result, error) { return reconcile.Result{RequeueAfter: r.syncInterval}, nil }
	failure := func(err error) (reconcile.Result, error) { return reconcile.Result{}, err }

	if !u.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if u.Status.CurrentSpec != nil {
			l.Info("delete user (status set)")

			l.Info("get tenant admin client")
			tr := u.Status.CurrentSpec.TenantRef.SetDefaultNamespace(u.GetNamespace())
			mtci, err := getMinioTenantClientInfo(ctx, r, tr)
			if err != nil {
				return failure(err)
			}
			mtac, err := mtci.GetAdminClient(ctx)
			if err != nil {
				return failure(err)
			}

			l.Info("delete minio user")
			err = mtac.RemoveUser(ctx, u.Status.CurrentSpec.AccessKey)
			err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchUser")
			if err != nil {
				return failure(err)
			}

			l.Info("clear status")
			u.Status.CurrentSpec = nil
			err = r.Update(ctx, u)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(u, finalizer)
		err = r.Update(ctx, u)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if !controllerutil.ContainsFinalizer(u, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(u, finalizer)
		err = r.Update(ctx, u)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	if u.Status.CurrentSpec != nil {
		l.Info("check for user change")

		l.Info("get tenant admin client")
		tr := u.Status.CurrentSpec.TenantRef.SetDefaultNamespace(u.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("get minio user")
		_, err = mtac.GetUserInfo(ctx, u.Status.CurrentSpec.AccessKey)
		e := !isMadminErrorCode(err, "XMinioAdminNoSuchUser")
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchUser")
		if err != nil {
			return failure(err)
		}
		if !e {
			l.Info("clear status (minio user no longer exists)")
			u.Status.CurrentSpec = nil
			err = r.Update(ctx, u)
			if err != nil {
				return failure(err)
			}

			return success()
		}

		l.Info("get secret from secret key ref")
		usrr := u.Status.CurrentSpec.SecretKeyRef.SetDefaultNamespace(req.Namespace)
		us := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: usrr.Name, Namespace: usrr.Namespace}, us)
		if err != nil {
			return failure(err)
		}
		if us.GetResourceVersion() != u.Status.CurrentSecretKeyRefResourceVersion {
			l.Info("update user (secret key ref change)")
			usk := string(us.Data[u.Status.CurrentSpec.SecretKeyRef.Key])
			err = mtac.AddUser(ctx, u.Status.CurrentSpec.AccessKey, usk)
			if err != nil {
				return failure(err)
			}

			l.Info("set status")
			u.Status.CurrentSecretKeyRefResourceVersion = us.GetResourceVersion()
			err = r.Update(ctx, u)
			if err != nil {
				return failure(err)
			}

			return success()
		}
	}

	if u.Status.CurrentSpec == nil {
		l.Info("create user (status unset)")

		l.Info("get tenant admin client")
		tr := u.Spec.TenantRef.SetDefaultNamespace(u.GetNamespace())
		mtci, err := getMinioTenantClientInfo(ctx, r, tr)
		if err != nil {
			return failure(err)
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return failure(err)
		}

		l.Info("check if minio user exists")
		_, err = mtac.GetUserInfo(ctx, u.Spec.AccessKey)
		if err == nil {
			err = fmt.Errorf("user %s already exists", u.Spec.AccessKey)
		}
		err = ignoreMadminErrorCode(err, "XMinioAdminNoSuchUser")
		if err != nil {
			return failure(err)
		}

		l.Info("get secret from secret key ref")
		usrr := u.Spec.SecretKeyRef.SetDefaultNamespace(req.Namespace)
		us := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: usrr.Name, Namespace: usrr.Namespace}, us)
		if err != nil {
			return failure(err)
		}
		usk := string(us.Data[u.Spec.SecretKeyRef.Key])

		l.Info("create minio user")
		err = mtac.AddUser(ctx, u.Spec.AccessKey, usk)
		if err != nil {
			return failure(err)
		}

		l.Info("set status")
		u.Status.CurrentSpec = &u.Spec
		u.Status.CurrentSecretKeyRefResourceVersion = us.GetResourceVersion()
		err = r.Update(ctx, u)
		if err != nil {
			return failure(err)
		}

		return success()
	}

	return success()
}

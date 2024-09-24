package operator

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"

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
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	finalizer = "bfiola.dev/minio-operator-ext"
)

type Operator interface {
	Health() error
	Run() error
}

type operator struct {
	manager manager.Manager
	logger  *slog.Logger
}

type OperatorOpts struct {
	KubeConfig string
	Logger     *slog.Logger
}

func NewOperator(o *OperatorOpts) (*operator, error) {
	l := o.Logger
	if l == nil {
		l = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	c, err := clientcmd.BuildConfigFromFlags("", o.KubeConfig)
	if err != nil {
		return nil, err
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
	err = corev1.AddToScheme(s)
	if err != nil {
		return nil, err
	}
	m, err := manager.New(c, manager.Options{
		Logger: logr.FromSlogHandler(l.Handler()),
		Scheme: s,
	})
	if err != nil {
		return nil, err
	}

	op := &operator{
		logger:  l,
		manager: m,
	}

	err = (&minioBucketReconciler{}).register(m)
	if err != nil {
		return nil, err
	}
	err = (&minioGroupReconciler{}).register(m)
	if err != nil {
		return nil, err
	}
	err = (&minioGroupBindingReconciler{}).register(m)
	if err != nil {
		return nil, err
	}
	err = (&minioPolicyReconciler{}).register(m)
	if err != nil {
		return nil, err
	}
	err = (&minioPolicyBindingReconciler{}).register(m)
	if err != nil {
		return nil, err
	}
	err = (&minioUserReconciler{}).register(m)
	if err != nil {
		return nil, err
	}

	return op, err
}

func (o *operator) Health() error {
	return nil
}

func (o *operator) Run() error {
	o.logger.Info("starting operator")
	return o.manager.Start(context.Background())
}

type minioTenantClientInfo struct {
	AccessKey string
	CaBundle  *x509.CertPool
	Endpoint  string
	SecretKey string
	Secure    bool
}

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
		return nil, fmt.Errorf("key %s in %s/%s/%s", k, ts.Namespace, ts.Name, tsk)
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
	for i := range tsvc.Spec.Ports {
		p := tsvc.Spec.Ports[i]
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

func (mtci *minioTenantClientInfo) GetAdminClient(ctx context.Context, rr v1.ResourceRef) (*madmin.AdminClient, error) {
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

type minioBucketReconciler struct {
	client.Client
	logger logr.Logger
}

func (r *minioBucketReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioBucket{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
}

func (r *minioBucketReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	b := &v1.MinioBucket{}
	err := r.Get(ctx, req.NamespacedName, b)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	deleteBucket := func() error {
		l.Info("get tenant client")
		mtci, err := getMinioTenantClientInfo(ctx, r, *b.Status.TenantRef)
		if err != nil {
			return err
		}
		mtc, err := mtci.GetClient(ctx)
		if err != nil {
			return err
		}

		l.Info("delete minio bucket")
		err = mtc.RemoveBucket(ctx, *b.Status.Name)
		if err != nil {
			mcerr, ok := err.(minioclient.ErrorResponse)
			if !ok {
				return err
			}
			if mcerr.Code != "NoSuchBucket" {
				return err
			}
		}

		l.Info("clear status")
		b.Status.Name = nil
		b.Status.TenantRef = nil
		err = r.Status().Update(ctx, b)
		if err != nil {
			return err
		}

		return nil
	}

	if !b.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")
		if b.Status.TenantRef != nil && b.Status.Name != nil {
			l.Info("delete bucket (status set)")
			err := deleteBucket()
			if err != nil {
				return reconcile.Result{}, err
			}
		}

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(b, finalizer)
		err = r.Update(ctx, b)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(b, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(b, finalizer)
		err = r.Update(ctx, b)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if (b.Status.Name != nil && b.Status.TenantRef != nil) && (*b.Status.Name != b.Spec.Name || *b.Status.TenantRef != b.Spec.TenantRef) {
		l.Info("re-create bucket (status and spec differ)")
		err := deleteBucket()
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if b.Status.Name == nil && b.Status.TenantRef == nil {
		l.Info("create bucket (status unset)")

		l.Info("get tenant client")
		mtci, err := getMinioTenantClientInfo(ctx, r, b.Spec.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtc, err := mtci.GetClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("create minio bucket")
		err = mtc.MakeBucket(ctx, b.Spec.Name, minioclient.MakeBucketOptions{})
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("set status")
		b.Status.Name = &b.Spec.Name
		b.Status.TenantRef = &b.Spec.TenantRef
		err = r.Status().Update(ctx, b)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

type minioGroupReconciler struct {
	client.Client
	logger logr.Logger
}

func (r *minioGroupReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioGroup{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
}

func (r *minioGroupReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	g := &v1.MinioGroup{}
	err := r.Get(ctx, req.NamespacedName, g)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !g.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(g, finalizer)
		err = r.Update(ctx, g)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(g, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(g, finalizer)
		err = r.Update(ctx, g)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

type minioGroupBindingReconciler struct {
	client.Client
	logger logr.Logger
}

func (r *minioGroupBindingReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioGroupBinding{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
}

func (r *minioGroupBindingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	gb := &v1.MinioGroup{}
	err := r.Get(ctx, req.NamespacedName, gb)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !gb.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(gb, finalizer)
		err = r.Update(ctx, gb)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(gb, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(gb, finalizer)
		err = r.Update(ctx, gb)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

type minioPolicyReconciler struct {
	client.Client
	logger logr.Logger
}

func (r *minioPolicyReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioPolicy{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
}

func (r *minioPolicyReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	p := &v1.MinioGroup{}
	err := r.Get(ctx, req.NamespacedName, p)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !p.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(p, finalizer)
		err = r.Update(ctx, p)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(p, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(p, finalizer)
		err = r.Update(ctx, p)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

type minioPolicyBindingReconciler struct {
	client.Client
	logger logr.Logger
}

func (r *minioPolicyBindingReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioPolicyBinding{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
}

func (r *minioPolicyBindingReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	pb := &v1.MinioGroup{}
	err := r.Get(ctx, req.NamespacedName, pb)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !pb.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(pb, finalizer)
		err = r.Update(ctx, pb)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(pb, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(pb, finalizer)
		err = r.Update(ctx, pb)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

type minioUserReconciler struct {
	client.Client
	logger logr.Logger
}

func (r *minioUserReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioUser{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
}

func (r *minioUserReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	l := r.logger.WithValues("resource", req.NamespacedName.String())
	l.Info("reconcile")

	u := &v1.MinioGroup{}
	err := r.Get(ctx, req.NamespacedName, u)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !u.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		l.Info("clear finalizer")
		controllerutil.RemoveFinalizer(u, finalizer)
		err = r.Update(ctx, u)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(u, finalizer) {
		l.Info("add finalizer")
		controllerutil.AddFinalizer(u, finalizer)
		err = r.Update(ctx, u)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

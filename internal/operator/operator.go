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

	err = builder.ControllerManagedBy(m).For(&v1.MinioBucket{}).Complete(reconcile.Func(op.reconcileMinioBucket))
	if err != nil {
		return nil, err
	}
	err = builder.ControllerManagedBy(m).For(&v1.MinioGroup{}).Complete(reconcile.Func(op.reconcileMinioGroup))
	if err != nil {
		return nil, err
	}
	err = builder.ControllerManagedBy(m).For(&v1.MinioGroupBinding{}).Complete(reconcile.Func(op.reconcileMinioGroupBinding))
	if err != nil {
		return nil, err
	}
	err = builder.ControllerManagedBy(m).For(&v1.MinioPolicy{}).Complete(reconcile.Func(op.reconcileMinioPolicy))
	if err != nil {
		return nil, err
	}
	err = builder.ControllerManagedBy(m).For(&v1.MinioPolicyBinding{}).Complete(reconcile.Func(op.reconcileMinioPolicyBinding))
	if err != nil {
		return nil, err
	}
	err = builder.ControllerManagedBy(m).For(&v1.MinioUser{}).Complete(reconcile.Func(op.reconcileMinioUser))
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

type tenantClientInfo struct {
	AccessKey string
	CaBundle  []byte
	Endpoint  string
	SecretKey string
	Secure    bool
}

func (o *operator) getMinioTenantClientInfo(ctx context.Context, rr v1.ResourceRef) (*tenantClientInfo, error) {
	if rr.Namespace == "" {
		return nil, fmt.Errorf("namespace empty")
	}

	c := o.manager.GetClient()

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

	return &tenantClientInfo{
		AccessKey: ak,
		CaBundle:  []byte(cb),
		Endpoint:  e,
		Secure:    s,
		SecretKey: sk,
	}, nil
}

func (o *operator) getMinioTenantClient(ctx context.Context, rr v1.ResourceRef) (*minioclient.Client, error) {
	mtci, err := o.getMinioTenantClientInfo(ctx, rr)
	if err != nil {
		return nil, err
	}

	cp := x509.NewCertPool()
	cpb, _ := pem.Decode(mtci.CaBundle)
	c, err := x509.ParseCertificate(cpb.Bytes)
	if err != nil {
		return nil, err
	}
	cp.AddCert(c)

	mtc, err := minioclient.New(mtci.Endpoint, &minioclient.Options{
		Creds:  miniocredentials.NewStaticV4(mtci.AccessKey, mtci.SecretKey, ""),
		Secure: mtci.Secure,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: cp,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	return mtc, nil
}

func (o *operator) getMinioTenantAdminClient(ctx context.Context, rr v1.ResourceRef) (*madmin.AdminClient, error) {
	mtci, err := o.getMinioTenantClientInfo(ctx, rr)
	if err != nil {
		return nil, err
	}

	cp := x509.NewCertPool()
	cp.AddCert(&x509.Certificate{
		Raw: []byte(mtci.CaBundle),
	})

	mtac, err := madmin.New(mtci.Endpoint, mtci.AccessKey, mtci.SecretKey, mtci.Secure)
	mtac.SetCustomTransport(&http.Transport{
		TLSClientConfig: &tls.Config{
			ClientCAs: cp,
		},
	})
	if err != nil {
		return nil, err
	}

	return mtac, nil
}

func (o *operator) reconcileMinioBucket(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	o.logger.Info("bucket reconciliation")

	c := o.manager.GetClient()

	// get bucket
	b := &v1.MinioBucket{}
	err := c.Get(ctx, req.NamespacedName, b)
	if err != nil {
		return reconcile.Result{}, err
	}

	// ensure ref has namespace
	rr := b.Spec.TenantRef.SetDefaultNamespace(req.Namespace)

	// get client
	mtc, err := o.getMinioTenantClient(ctx, rr)
	if err != nil {
		return reconcile.Result{}, err
	}

	hf := controllerutil.ContainsFinalizer(b, finalizer)
	if hf {
		// bucket was previously created
		if b.ObjectMeta.DeletionTimestamp.IsZero() {
			// handle resource deletion
			err := mtc.RemoveBucket(ctx, b.Spec.Name)
			if err != nil {
				return reconcile.Result{}, err
			}

			controllerutil.RemoveFinalizer(b, finalizer)
			err = c.Update(ctx, b)
			if err != nil {
				return reconcile.Result{}, err
			}

			return reconcile.Result{}, nil
		}
		// handle updates
		return reconcile.Result{}, nil
	}

	// handle creation
	err = mtc.MakeBucket(ctx, b.Spec.Name, minioclient.MakeBucketOptions{})
	if err != nil {
		return reconcile.Result{}, err
	}

	controllerutil.AddFinalizer(b, finalizer)
	err = c.Update(ctx, b)
	if err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (o *operator) reconcileMinioGroup(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	o.logger.Info("group reconciliation")
	return reconcile.Result{}, nil
}

func (o *operator) reconcileMinioGroupBinding(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	o.logger.Info("group binding reconciliation")
	return reconcile.Result{}, nil
}

func (o *operator) reconcileMinioPolicy(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	o.logger.Info("policy reconciliation")
	return reconcile.Result{}, nil
}

func (o *operator) reconcileMinioPolicyBinding(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	o.logger.Info("policy reconciliation")
	return reconcile.Result{}, nil
}

func (o *operator) reconcileMinioUser(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	o.logger.Info("user reconciliation")
	return reconcile.Result{}, nil
}

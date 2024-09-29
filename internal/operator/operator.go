package operator

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	kscheme "k8s.io/client-go/kubernetes/scheme"
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
	KubeConfig string
	Logger     *slog.Logger
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
		Logger: logr.FromSlogHandler(l.Handler()),
		Scheme: s,
	})
	if err != nil {
		return nil, err
	}

	rs := []reconciler{
		&minioBucketReconciler{},
		&minioGroupReconciler{},
		&minioGroupBindingReconciler{},
		&minioPolicyReconciler{},
		&minioPolicyBindingReconciler{},
		&minioUserReconciler{},
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
	return o.manager.Start(context.Background())
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
	logger logr.Logger
}

// Builds a controller with a [minioBucketReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioBucketReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioBucket{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
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

	if !b.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")
		if b.Status.CurrentSpec != nil {
			l.Info("delete bucket (status set)")

			l.Info("get tenant client")
			mtci, err := getMinioTenantClientInfo(ctx, r, b.Status.CurrentSpec.TenantRef)
			if err != nil {
				return reconcile.Result{}, err
			}
			mtc, err := mtci.GetClient(ctx)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("delete minio bucket")
			err = mtc.RemoveBucket(ctx, b.Status.CurrentSpec.Name)
			if err != nil {
				mcerr, ok := err.(minioclient.ErrorResponse)
				if !ok {
					return reconcile.Result{}, err
				}
				if mcerr.Code != "NoSuchBucket" {
					return reconcile.Result{}, err
				}
			}

			l.Info("clear status")
			b.Status.CurrentSpec = nil
			err = r.Update(ctx, b)
			if err != nil {
				return reconcile.Result{}, err
			}

			return reconcile.Result{}, nil
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

	if b.Status.CurrentSpec == nil {
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
		b.Status.CurrentSpec = &b.Spec
		err = r.Update(ctx, b)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

// minioGroupReconciler reconciles [v1.MinioGroup] resources
type minioGroupReconciler struct {
	client.Client
	logger logr.Logger
}

// Builds a controller with a [minioGroupReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioGroupReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioGroup{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
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

	if !g.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if g.Status.CurrentSpec != nil {
			l.Info("delete group (status set)")

			l.Info("get tenant admin client")
			mtci, err := getMinioTenantClientInfo(ctx, r, g.Status.CurrentSpec.TenantRef)
			if err != nil {
				return reconcile.Result{}, err
			}
			mtac, err := mtci.GetAdminClient(ctx)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("delete minio group")
			err = mtac.UpdateGroupMembers(ctx, madmin.GroupAddRemove{
				Group:    g.Status.CurrentSpec.Name,
				IsRemove: true,
			})
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("clear status")
			g.Status.CurrentSpec = nil
			err = r.Update(ctx, g)
			if err != nil {
				return reconcile.Result{}, err
			}

			return reconcile.Result{}, nil
		}

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

	if g.Status.CurrentSpec == nil {
		l.Info("create group (status unset)")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, g.Spec.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("create minio group")
		err = mtac.UpdateGroupMembers(ctx, madmin.GroupAddRemove{
			Group:    g.Spec.Name,
			IsRemove: false,
		})
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("set status")
		g.Status.CurrentSpec = &g.Spec
		err = r.Update(ctx, g)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

// minioGroupBindingReconciler reconciles [v1.MinioGroupBinding] resources
type minioGroupBindingReconciler struct {
	client.Client
	logger logr.Logger
}

// Builds a controller with a [minioGroupBindingReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioGroupBindingReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioGroupBinding{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
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

	deleteGroupMember := func() error {
		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, gb.Status.CurrentSpec.TenantRef)
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
				return reconcile.Result{}, err
			}

			return reconcile.Result{}, nil
		}

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

	if gb.Status.CurrentSpec != nil && (gb.Status.CurrentSpec.Group != gb.Spec.Group || gb.Status.CurrentSpec.User != gb.Spec.User) {
		l.Info("delete group member (status and spec differ)")

		err = deleteGroupMember()
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if gb.Status.CurrentSpec == nil {
		l.Info("add group member (status unset)")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, gb.Spec.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("add minio group member")
		err = mtac.UpdateGroupMembers(ctx, madmin.GroupAddRemove{
			Group:    gb.Spec.Group,
			Members:  []string{gb.Spec.User},
			IsRemove: false,
		})
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("set status")
		gb.Status.CurrentSpec = &gb.Spec
		err = r.Update(ctx, gb)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil

	}

	return reconcile.Result{}, nil
}

// minioPolicyReconciler reconciles [v1.MinioPolicy] resources
type minioPolicyReconciler struct {
	client.Client
	logger logr.Logger
}

// Builds a controller with a [minioPolicyReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioPolicyReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioPolicy{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
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

	if !p.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if p.Status.CurrentSpec != nil {
			l.Info("delete policy (status set)")

			l.Info("get tenant client")
			mtci, err := getMinioTenantClientInfo(ctx, r, p.Status.CurrentSpec.TenantRef)
			if err != nil {
				return reconcile.Result{}, err
			}
			mtac, err := mtci.GetAdminClient(ctx)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("delete minio policy")
			err = mtac.RemoveCannedPolicy(ctx, p.Status.CurrentSpec.Name)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("clear status")
			p.Status.CurrentSpec = nil
			err = r.Update(ctx, p)
			if err != nil {
				return reconcile.Result{}, err
			}

			return reconcile.Result{}, nil
		}

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

	if p.Status.CurrentSpec != nil {
		l.Info("check for policy change")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, p.Status.CurrentSpec.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("get policy")
		mp, err := mtac.InfoCannedPolicyV2(ctx, p.Status.CurrentSpec.Name)
		if err != nil {
			return reconcile.Result{}, err
		}
		if mp != nil {
			return reconcile.Result{}, fmt.Errorf("unimplemented")
		}

		return reconcile.Result{}, fmt.Errorf("unimplemented")
	}

	if p.Status.CurrentSpec == nil {
		l.Info("create policy (status unset)")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, p.Spec.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("marshal policy to json")
		pd, err := json.Marshal(map[string]any{
			"statement": p.Spec.Statement,
			"version":   p.Spec.Version,
		})
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("create minio policy")
		err = mtac.AddCannedPolicy(ctx, p.Spec.Name, pd)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("set status")
		p.Status.CurrentSpec = &p.Spec
		err = r.Update(ctx, p)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

// minioPolicyBindingReconciler reconciles [v1.MinioPolicyBinding] resources
type minioPolicyBindingReconciler struct {
	client.Client
	logger logr.Logger
}

// Builds a controller with a [minioPolicyBindingReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioPolicyBindingReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioPolicyBinding{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
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

	detachPolicyMember := func() error {
		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, pb.Status.CurrentSpec.TenantRef)
		if err != nil {
			return err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return err
		}

		l.Info("detach minio policy from identity")
		req := madmin.PolicyAssociationReq{
			Policies: []string{pb.Status.CurrentSpec.Policy},
		}
		ldap := false
		if pb.Status.CurrentSpec.Group.Ldap != nil {
			ldap = true
			req.Group = *pb.Status.CurrentSpec.Group.Ldap
		} else if pb.Status.CurrentSpec.User.Ldap != nil {
			ldap = true
			req.User = *pb.Status.CurrentSpec.User.Ldap
		} else if pb.Status.CurrentSpec.Group.Builtin != nil {
			req.Group = *pb.Status.CurrentSpec.Group.Builtin
		} else if pb.Status.CurrentSpec.User.Builtin != nil {
			req.User = *pb.Status.CurrentSpec.User.Builtin
		}
		if ldap {
			_, err = mtac.DetachPolicyLDAP(ctx, req)
		} else {
			_, err = mtac.DetachPolicy(ctx, req)
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
				return reconcile.Result{}, err
			}

			return reconcile.Result{}, nil
		}

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

	val := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}

	if (pb.Status.CurrentSpec != nil) && (val(pb.Status.CurrentSpec.Group.Builtin) != val(pb.Spec.Group.Builtin) ||
		val(pb.Status.CurrentSpec.Group.Ldap) != val(pb.Spec.Group.Ldap) ||
		val(pb.Status.CurrentSpec.User.Builtin) != val(pb.Spec.User.Builtin) ||
		val(pb.Status.CurrentSpec.User.Ldap) != val(pb.Spec.User.Ldap)) {
		l.Info("detach policy member (status and spec differ)")

		err = detachPolicyMember()
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if pb.Status.CurrentSpec == nil {
		l.Info("attach policy member (status unset)")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, pb.Spec.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("attach minio policy to identity")
		req := madmin.PolicyAssociationReq{
			Policies: []string{pb.Spec.Policy},
		}
		ldap := false
		if pb.Spec.Group.Ldap != nil {
			ldap = true
			req.Group = *pb.Spec.Group.Ldap
		} else if pb.Spec.User.Ldap != nil {
			ldap = true
			req.User = *pb.Spec.User.Ldap
		} else if pb.Spec.Group.Builtin != nil {
			req.Group = *pb.Spec.Group.Builtin
		} else if pb.Spec.User.Builtin != nil {
			req.User = *pb.Spec.User.Builtin
		}
		if ldap {
			_, err = mtac.AttachPolicyLDAP(ctx, req)
		} else {
			_, err = mtac.AttachPolicy(ctx, req)
		}
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("set status")
		pb.Status.CurrentSpec = &pb.Spec
		err = r.Update(ctx, pb)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

// minioUserReconciler reconciles [v1.MinioUser] resources
type minioUserReconciler struct {
	client.Client
	logger logr.Logger
}

// Builds a controller with a [minioUserReconciler].
// Registers this controller with a [manager.Manager] instance.
// Returns an error if a controller cannot be built.
func (r *minioUserReconciler) register(m manager.Manager) error {
	r.Client = m.GetClient()
	ctrl, err := builder.ControllerManagedBy(m).For(&v1.MinioUser{}).Build(r)
	r.logger = ctrl.GetLogger()
	return err
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

	if !u.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if u.Status.CurrentSpec != nil {
			l.Info("delete user (status set)")

			l.Info("get tenant admin client")
			mtci, err := getMinioTenantClientInfo(ctx, r, u.Status.CurrentSpec.TenantRef)
			if err != nil {
				return reconcile.Result{}, err
			}
			mtac, err := mtci.GetAdminClient(ctx)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("delete minio user")
			err = mtac.RemoveUser(ctx, u.Status.CurrentSpec.AccessKey)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("clear status")
			u.Status.CurrentSpec = nil
			err = r.Update(ctx, u)
			if err != nil {
				return reconcile.Result{}, err
			}

			return reconcile.Result{}, nil
		}

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

	if u.Status.CurrentSpec != nil {
		l.Info("check for user change")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, u.Status.CurrentSpec.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("get user")
		ui, err := mtac.GetUserInfo(ctx, u.Status.CurrentSpec.AccessKey)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("get secret from secret key ref")
		usrr := u.Spec.SecretKeyRef.SetDefaultNamespace(req.Namespace)
		us := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: usrr.Name, Namespace: usrr.Namespace}, us)
		if err != nil {
			return reconcile.Result{}, err
		}
		usk := string(us.Data[u.Spec.SecretKeyRef.Key])

		if ui.SecretKey != usk {
			l.Info("update user (secret key change)")
			err = mtac.AddUser(ctx, u.Spec.AccessKey, usk)
			if err != nil {
				return reconcile.Result{}, err
			}

			return reconcile.Result{}, nil
		}
	}

	if u.Status.CurrentSpec == nil {
		l.Info("create user (status unset)")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, u.Spec.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("get secret from secret key ref")
		usrr := u.Spec.SecretKeyRef.SetDefaultNamespace(req.Namespace)
		us := &corev1.Secret{}
		err = r.Get(ctx, types.NamespacedName{Name: usrr.Name, Namespace: usrr.Namespace}, us)
		if err != nil {
			return reconcile.Result{}, err
		}
		usk := string(us.Data[u.Spec.SecretKeyRef.Key])

		l.Info("create minio user")
		err = mtac.AddUser(ctx, u.Spec.AccessKey, usk)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("set status")
		u.Status.CurrentSpec = &u.Spec
		err = r.Update(ctx, u)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

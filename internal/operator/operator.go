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

	if !b.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")
		if b.Status.TenantRef != nil && b.Status.Name != nil {
			l.Info("delete bucket (status set)")

			l.Info("get tenant client")
			mtci, err := getMinioTenantClientInfo(ctx, r, *b.Status.TenantRef)
			if err != nil {
				return reconcile.Result{}, err
			}
			mtc, err := mtci.GetClient(ctx)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("delete minio bucket")
			err = mtc.RemoveBucket(ctx, *b.Status.Name)
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
			b.Status.Name = nil
			b.Status.TenantRef = nil
			err = r.Status().Update(ctx, b)
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

		if g.Status.TenantRef != nil && g.Status.Name != nil {
			l.Info("delete group (status set)")

			l.Info("get tenant admin client")
			mtci, err := getMinioTenantClientInfo(ctx, r, *g.Status.TenantRef)
			if err != nil {
				return reconcile.Result{}, err
			}
			mtac, err := mtci.GetAdminClient(ctx)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("delete minio group")
			err = mtac.UpdateGroupMembers(ctx, madmin.GroupAddRemove{
				Group:    *g.Status.Name,
				IsRemove: true,
			})
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("clear status")
			g.Status.Name = nil
			g.Status.TenantRef = nil
			err = r.Status().Update(ctx, g)
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

	if g.Status.Name == nil && g.Status.TenantRef == nil {
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
		g.Status.Name = &g.Spec.Name
		g.Status.TenantRef = &g.Spec.TenantRef
		err = r.Status().Update(ctx, g)
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

	gb := &v1.MinioGroupBinding{}
	err := r.Get(ctx, req.NamespacedName, gb)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	deleteGroupMember := func() error {
		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, *gb.Status.TenantRef)
		if err != nil {
			return err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return err
		}

		l.Info("delete minio group member")
		gd, err := mtac.GetGroupDescription(ctx, *gb.Status.Group)
		if err != nil {
			return err
		}
		ms := []string{}
		for _, m := range gd.Members {
			if m == *gb.Status.User {
				continue
			}
			ms = append(ms, m)
		}
		err = mtac.UpdateGroupMembers(ctx, madmin.GroupAddRemove{
			Group:    *gb.Status.Group,
			Members:  ms,
			IsRemove: false,
		})
		if err != nil {
			return err
		}

		l.Info("clear status")
		gb.Status.Group = nil
		gb.Status.TenantRef = nil
		gb.Status.User = nil
		err = r.Status().Update(ctx, gb)
		if err != nil {
			return err
		}

		return nil
	}

	if !gb.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if gb.Status.TenantRef != nil && gb.Status.User != nil && gb.Status.Group != nil {
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

	if (gb.Status.TenantRef != nil) && ((gb.Status.User != nil && *gb.Status.User != gb.Spec.User) || (gb.Status.Group != nil && *gb.Status.Group != gb.Spec.Group)) {
		l.Info("delete group member (status and spec differ)")

		err = deleteGroupMember()
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if gb.Status.TenantRef == nil && gb.Status.User == nil && gb.Status.Group == nil {
		l.Info("add group member (status unset)")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, *gb.Status.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("add minio group member")
		gd, err := mtac.GetGroupDescription(ctx, gb.Spec.Group)
		if err != nil {
			return reconcile.Result{}, err
		}
		ms := []string{}
		for _, m := range gd.Members {
			if m == gb.Spec.User {
				continue
			}
			ms = append(ms, m)
		}
		ms = append(ms, gb.Spec.User)
		err = mtac.UpdateGroupMembers(ctx, madmin.GroupAddRemove{
			Group:    *gb.Status.Group,
			Members:  ms,
			IsRemove: false,
		})
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("set status")
		gb.Status.Group = &gb.Spec.Group
		gb.Status.TenantRef = gb.Spec.TenantRef
		gb.Status.User = &gb.Spec.User
		err = r.Status().Update(ctx, gb)
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

	p := &v1.MinioPolicy{}
	err := r.Get(ctx, req.NamespacedName, p)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !p.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if p.Status.TenantRef != nil && p.Status.Name != nil {
			l.Info("delete policy (status set)")

			l.Info("get tenant client")
			mtci, err := getMinioTenantClientInfo(ctx, r, *p.Status.TenantRef)
			if err != nil {
				return reconcile.Result{}, err
			}
			mtac, err := mtci.GetAdminClient(ctx)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("delete minio policy")
			err = mtac.DeletePolicy(ctx, *p.Status.Name)
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

	if p.Status.TenantRef != nil && p.Status.Name != nil {
		l.Info("check for policy change")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, *p.Status.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("get policy")
		mp, err := mtac.InfoCannedPolicyV2(ctx, *p.Status.Name)
		if err != nil {
			return reconcile.Result{}, err
		}
		if mp != nil {
			return reconcile.Result{}, fmt.Errorf("unimplemented")
		}

		return reconcile.Result{}, fmt.Errorf("unimplemented")
	}

	if p.Status.TenantRef == nil && p.Status.Name == nil {
		l.Info("create policy (status unset)")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, *p.Status.TenantRef)
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
		p.Status.Name = &p.Spec.Name
		p.Status.TenantRef = &p.Spec.TenantRef
		err = r.Status().Update(ctx, p)
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

	pb := &v1.MinioPolicyBinding{}
	err := r.Get(ctx, req.NamespacedName, pb)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	detachPolicyMember := func() error {
		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, *pb.Status.TenantRef)
		if err != nil {
			return err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return err
		}

		l.Info("detach minio policy from identity")
		req := madmin.PolicyAssociationReq{
			Policies: []string{*pb.Status.Policy},
		}
		ldap := false
		if pb.Status.Group.Ldap != nil {
			ldap = true
			req.Group = *pb.Status.Group.Ldap
		} else if pb.Status.User.Ldap != nil {
			ldap = true
			req.User = *pb.Status.User.Ldap
		} else if pb.Status.Group.Builtin != nil {
			req.Group = *pb.Status.Group.Builtin
		} else if pb.Status.User.Builtin != nil {
			req.User = *pb.Status.User.Builtin
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
		pb.Status.Group = nil
		pb.Status.Policy = nil
		pb.Status.TenantRef = nil
		pb.Status.User = nil
		err = r.Status().Update(ctx, pb)
		if err != nil {
			return err
		}

		return nil
	}

	if !pb.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if pb.Status.Group != nil && pb.Status.Policy != nil && pb.Status.TenantRef != nil && pb.Status.User != nil {
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

	if (pb.Status.TenantRef != nil) && ((pb.Status.Group != nil && *pb.Status.Group != pb.Spec.Group) || (pb.Status.Policy != nil && *pb.Status.Policy != pb.Spec.Policy) || (pb.Status.User != nil && *pb.Status.User != pb.Spec.User)) {
		l.Info("detach policy member (status and spec differ)")

		err = detachPolicyMember()
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	if pb.Status.TenantRef == nil && pb.Status.Group != nil && pb.Status.Policy != nil && pb.Status.User == nil {
		l.Info("attach policy member (status unset)")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, *pb.Status.TenantRef)
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
		if *pb.Spec.Group.Ldap != "" {
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
		pb.Status.Group = &pb.Spec.Group
		pb.Status.Policy = &pb.Spec.Policy
		pb.Status.TenantRef = &pb.Spec.TenantRef
		pb.Status.User = &pb.Spec.User
		err = r.Status().Update(ctx, pb)
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

	u := &v1.MinioUser{}
	err := r.Get(ctx, req.NamespacedName, u)
	if err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if !u.ObjectMeta.DeletionTimestamp.IsZero() {
		l.Info("marked for deletion")

		if u.Status.TenantRef != nil && u.Status.AccessKey != nil {
			l.Info("delete user (status set)")

			l.Info("get tenant admin client")
			mtci, err := getMinioTenantClientInfo(ctx, r, *u.Status.TenantRef)
			if err != nil {
				return reconcile.Result{}, err
			}
			mtac, err := mtci.GetAdminClient(ctx)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("delete minio user")
			err = mtac.RemoveUser(ctx, *u.Status.AccessKey)
			if err != nil {
				return reconcile.Result{}, err
			}

			l.Info("clear status")
			u.Status.AccessKey = nil
			u.Status.TenantRef = nil
			err = r.Status().Update(ctx, u)
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

	if u.Status.AccessKey != nil && u.Status.TenantRef != nil {
		l.Info("check for user change")

		l.Info("get tenant admin client")
		mtci, err := getMinioTenantClientInfo(ctx, r, *u.Status.TenantRef)
		if err != nil {
			return reconcile.Result{}, err
		}
		mtac, err := mtci.GetAdminClient(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}

		l.Info("get user")
		ui, err := mtac.GetUserInfo(ctx, *u.Status.AccessKey)
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
			err = mtac.SetUserReq(ctx, u.Spec.AccessKey, madmin.AddOrUpdateUserReq{SecretKey: usk})
			if err != nil {
				return reconcile.Result{}, err
			}

			return reconcile.Result{}, nil
		}
	}
	if u.Status.AccessKey == nil && u.Status.TenantRef == nil {
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
		u.Status.AccessKey = &u.Spec.AccessKey
		u.Status.TenantRef = &u.Spec.TenantRef
		err = r.Status().Update(ctx, u)
		if err != nil {
			return reconcile.Result{}, err
		}

		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

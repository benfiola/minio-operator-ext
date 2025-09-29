<<<<<<< HEAD
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
=======
>>>>>>> 38bcac6 (Split internal/operator/operator.go into multiple files)
package operator

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"strings"

	v1 "github.com/benfiola/minio-operator-ext/pkg/api/bfiola.dev/v1"
	"github.com/hashicorp/go-envparse"
	"github.com/minio/madmin-go/v3"
	minioclient "github.com/minio/minio-go/v7"
	miniocredentials "github.com/minio/minio-go/v7/pkg/credentials"
	miniov2 "github.com/minio/operator/pkg/apis/minio.min.io/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// minioTenantClientInfoOpts defines additional data used to modify the behavior around fetching tenant client information
type minioTenantClientInfoOpts struct {
	minioOperatorNamespace string
}

// minioTenantClientInfo defines the data required to instantiate a minio client from a given tenant.
type minioTenantClientInfo struct {
	AccessKey string
	CaBundle  *x509.CertPool
	Endpoint  string
	SecretKey string
	Secure    bool
}

// +kubebuilder:rbac:groups=core,resources=services,verbs=get
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list
// +kubebuilder:rbac:groups=minio.min.io,resources=tenants,verbs=get

// Returns [minioTenantClientInfo] for a [miniov2.Tenant] referenced by [v1.ResourceRef].
// Returns an error if unable to fetch tenant information.
func getMinioTenantClientInfo(ctx context.Context, c client.Client, rr v1.ResourceRef, opts minioTenantClientInfoOpts) (*minioTenantClientInfo, error) {
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
	s := false
	if t.Spec.RequestAutoCert != nil && *t.Spec.RequestAutoCert {
		s = true
	} else if len(t.Spec.ExternalCertSecret) > 0 {
		s = true
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

	// create cert pool for tls verification
	cbp, err := x509.SystemCertPool()
	if err != nil {
		cbp = x509.NewCertPool()
	}
	ks := []string{"public.crt", "tls.crt", "ca.crt"}
	// pod CA
	pcacm := &corev1.ConfigMap{}
	err = c.Get(ctx, types.NamespacedName{Namespace: rr.Namespace, Name: "kube-root-ca.crt"}, pcacm)
	if err == nil {
		for _, k := range ks {
			cas, ok := pcacm.Data[k]
			if !ok {
				continue
			}
			ca := []byte(cas)
			cbp.AppendCertsFromPEM(ca)
		}
	}
	// CAs from secrets in minio operator namespace prefixed with 'operator-ca-tls'
	sl := &corev1.SecretList{}
	err = c.List(ctx, sl, client.InNamespace(opts.minioOperatorNamespace))
	if err == nil {
		for _, s := range sl.Items {
			if !strings.HasPrefix(s.Name, "operator-ca-tls") {
				continue
			}
			for _, k := range ks {
				cas, ok := s.Data[k]
				if !ok {
					continue
				}
				ca := []byte(cas)
				cbp.AppendCertsFromPEM(ca)
			}
		}
	}
	// CAs from configured external sources deployed in minio operator namespace
	for _, eca := range t.Spec.ExternalCaCertSecret {
		if eca.Type == "kubernetes.io/tls" {
			s := &corev1.Secret{}
			err := c.Get(ctx, types.NamespacedName{Namespace: opts.minioOperatorNamespace, Name: eca.Name}, s)
			if err != nil {
				continue
			}
			for _, k := range ks {
				cas, ok := s.Data[k]
				if !ok {
					continue
				}
				ca := []byte(cas)
				cbp.AppendCertsFromPEM(ca)
			}
		}
	}

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

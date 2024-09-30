package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

func run(ctx context.Context) {
	fmt.Printf("started\n")

	kc := ""
	for _, ev := range os.Environ() {
		k, v, _ := strings.Cut(ev, "=")
		if k != "KUBECONFIG" {
			continue
		}
		kc = v
	}

	fmt.Printf("kubeconfig: %s\n", kc)

	for {
		// handle cancellation
		cncl := false
		select {
		case <-ctx.Done():
			cncl = true
		default:
		}
		if cncl {
			break
		}

		time.Sleep(1 * time.Second)

		// get client config
		rc, err := clientcmd.BuildConfigFromFlags("", kc)
		if err != nil {
			fmt.Printf("error building k8s rest config: %s\n", err.Error())
			continue
		}

		// build client
		s := runtime.NewScheme()
		corev1.AddToScheme(s)
		c, err := client.New(rc, client.Options{Scheme: s})
		if err != nil {
			fmt.Printf("error building k8s client: %s\n", err.Error())
			continue
		}

		// query for load balancer services
		svcl := &corev1.ServiceList{}
		err = c.List(context.Background(), svcl, &client.ListOptions{})
		if err != nil {
			fmt.Printf("error getting load balancer list: %s\n", err.Error())
			continue
		}

		// build host -> ip mapping
		svcm := map[string]string{}
		for _, svc := range svcl.Items {
			if svc.Spec.Type != "LoadBalancer" {
				continue
			}
			if len(svc.Status.LoadBalancer.Ingress) == 0 {
				continue
			}
			dns := fmt.Sprintf("%s.%s.svc", svc.Name, svc.Namespace)
			ip := svc.Status.LoadBalancer.Ingress[0].IP
			svcm[dns] = ip
		}

		// read hosts file
		hf := "/etc/hosts"
		hb, err := os.ReadFile(hf)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				fmt.Printf("error reading hosts file: %s\n", err.Error())
				continue
			}
			hb = []byte("")
		}
		h := string(hb)

		// update existing entries in host file
		key := "# hosts-manager"
		ls := []string{}
		for _, l := range strings.Split(h, "\n") {
			lts := strings.TrimSpace(l)
			if !strings.HasSuffix(lts, key) {
				// line not managed by hosts-manager - keep
				ls = append(ls, l)
				continue
			}
			ps := strings.Split(lts, "\t")
			dns := ps[1]
			nip, ok := svcm[dns]
			if !ok {
				// dns record no longer maps to service - remove
				fmt.Printf("removing entry: %s\n", dns)
				continue
			}
			oip := ps[0]
			delete(svcm, dns)
			if oip == nip {
				// dns record unchanged - keep
				ls = append(ls, l)
				continue
			}
			//  update dns record
			fmt.Printf("updating entry: %s (%s)\n", dns, nip)
			nl := fmt.Sprintf("%s\t%s\t%s", nip, dns, key)
			ls = append(ls, nl)
		}

		// add new entries to host file
		for dns, ip := range svcm {
			fmt.Printf("adding entry: %s (%s)\n", dns, ip)
			nl := fmt.Sprintf("%s\t%s\t%s", ip, dns, key)
			ls = append(ls, nl)
		}

		d := []byte(strings.Join(ls, "\n"))
		err = os.WriteFile(hf, d, 0644)
		if err != nil {
			fmt.Printf("failed to write hosts file: %s\n", err.Error())
			continue
		}
	}

	fmt.Printf("finished\n")
}

func main() {
	log.SetLogger(logr.Discard())

	cancelChan := make(chan os.Signal, 1)
	signal.Notify(cancelChan, syscall.SIGTERM, syscall.SIGINT)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func(ctx context.Context) {
		defer wg.Done()
		run(ctx)
	}(ctx)

	sig := <-cancelChan
	fmt.Printf("caught signal %v - waiting for finish\n", sig)
	cancel()
	wg.Wait()
}

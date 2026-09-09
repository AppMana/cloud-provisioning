// Command peer-publisher runs in a pod and delivers public peer lists to the
// host tunnel service. It never reads the bootstrap private key or owns a NIC.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/peerpublisher"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnelhost"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	namespace := flag.String("namespace", "", "namespace of adoption Secret")
	machineFile := flag.String("machine-name-file", "", "host file containing the CAPI Machine name")
	request := flag.String("request-file", "", "public delivery request path")
	receipt := flag.String("receipt-file", "", "host application receipt path")
	token := flag.String("token-file", "", "projected service-account token path")
	ca := flag.String("ca-file", "", "projected Kubernetes CA path")
	server := flag.String("api-server", "", "API URL; defaults to Kubernetes service environment")
	poll := flag.Duration("poll-interval", 5*time.Second, "delivery polling interval")
	flag.Parse()
	if *namespace == "" || *machineFile == "" || *request == "" || *receipt == "" || *token == "" || *ca == "" || *poll < time.Second {
		log.Fatal("namespace, machine identity, delivery paths and projected credentials are required")
	}
	raw, err := os.ReadFile(*machineFile)
	if err != nil {
		log.Fatal("reading machine name: ", err)
	}
	machine := strings.TrimSpace(string(raw))
	if machine == "" {
		log.Fatal("empty Machine name")
	}
	if *server == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			log.Fatal("Kubernetes API endpoint missing")
		}
		*server = "https://" + net.JoinHostPort(host, port)
	}
	config := &rest.Config{Host: *server, BearerTokenFile: *token, TLSClientConfig: rest.TLSClientConfig{CAFile: *ca}, Timeout: 10 * time.Second}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatal(err)
	}
	publisher := peerpublisher.Publisher{Secrets: client.CoreV1().Secrets(*namespace), Name: tunnel.AdoptionSecretName(machine), Delivery: tunnelhost.DeliveryFiles{RequestPath: *request, ReceiptPath: *receipt, MaxReceiptAge: 30 * time.Second}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ticker := time.NewTicker(*poll)
	defer ticker.Stop()
	for {
		if err := publisher.Reconcile(ctx); err != nil {
			log.Printf("peer delivery not acknowledged: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

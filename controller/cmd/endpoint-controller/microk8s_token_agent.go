package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/join/microk8s"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// microk8sTokenAgentCommand runs on a MicroK8s control plane and keeps the
// join credential for MicroK8s workers current (microk8s.TokenAgent).
const microk8sTokenAgentCommand = "microk8s-token-agent"

func runMicrok8sTokenAgent(args []string) int {
	fs := flag.NewFlagSet(microk8sTokenAgentCommand, flag.ContinueOnError)
	namespace := fs.String("namespace", os.Getenv("POD_NAMESPACE"), "namespace of the join credential Secret")
	name := fs.String("secret-name", "microk8s-provider-config", "join credential Secret")
	node := fs.String("node-name", os.Getenv("NODE_NAME"), "the control plane this agent runs on")
	root := fs.String("snap-data-root", "/var/snap/microk8s", "the host's MicroK8s snap data directory")
	ttl := fs.Duration("token-ttl", 24*time.Hour, "lifetime of each join token; it is replaced at half of it")
	interval := fs.Duration("interval", time.Minute, "how often the credential is checked")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *namespace == "" || *node == "" || *ttl < 10*time.Minute {
		fmt.Fprintln(os.Stderr, "the namespace and node name are required, and the token lifetime must be at least 10m")
		return 2
	}
	ctrl.SetLogger(zap.New())
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	agent := &microk8s.TokenAgent{Client: c, Namespace: *namespace, Name: *name, NodeName: *node, Root: *root, TTL: *ttl, Now: time.Now}
	ctx := ctrl.SetupSignalHandler()
	if err := agent.Run(ctrl.LoggerInto(ctx, ctrl.Log.WithName(microk8sTokenAgentCommand)), *interval); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

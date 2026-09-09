// This program builds inside the pinned machine-config-operator source module.
// It invokes the distribution's complete Ignition handler without listening on
// a network socket. Run through authenticated Kubernetes exec, never as a Job
// whose stdout would expose bootstrap credentials in pod logs.
package main

import (
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"time"

	"github.com/openshift/machine-config-operator/pkg/server"
	"github.com/openshift/machine-config-operator/pkg/version"
)

func main() {
	api := flag.String("api-url", "", "cluster API URL embedded in the bootstrap kubeconfig")
	payload := flag.String("payload-version", "", "observed cluster release")
	timeout := flag.Duration("timeout", 45*time.Second, "hard export deadline")
	flag.Parse()
	if *api == "" || *payload == "" {
		fmt.Fprintln(os.Stderr, "api-url and payload-version are required")
		os.Exit(2)
	}
	version.ReleaseVersion = *payload
	done := make(chan error, 1)
	go func() {
		source, err := server.NewClusterServer("", *api)
		if err != nil {
			done <- err
			return
		}
		req := httptest.NewRequest("GET", "https://localhost/config/worker", nil)
		req.Header.Set("Accept", "application/vnd.coreos.ignition+json;version=3.5.0")
		response := httptest.NewRecorder()
		server.NewServerAPIHandler(source).ServeHTTP(response, req)
		if response.Code != 200 {
			done <- fmt.Errorf("MCS returned HTTP %d", response.Code)
			return
		}
		_, err = os.Stdout.Write(response.Body.Bytes())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case <-time.After(*timeout):
		fmt.Fprintln(os.Stderr, "Ignition export deadline exceeded")
		os.Exit(1)
	}
}

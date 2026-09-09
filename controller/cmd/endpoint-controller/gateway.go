package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	attachments "github.com/appmana/cloud-provisioning/controller/pkg/attachment/runtime"
	"github.com/aws/aws-sdk-go-v2/config"
	ctrl "sigs.k8s.io/controller-runtime"
)

func registerGateway(mgr ctrl.Manager, path, namespace, mesh, vip, port string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var scope attachments.AWSConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&scope); err != nil {
		return fmt.Errorf("AWS gateway configuration: %w", err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("unexpected trailing AWS gateway configuration")
	}
	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(scope.Scope.Region), config.WithRetryMaxAttempts(3))
	if err != nil {
		return err
	}
	return attachments.RegisterAWS(mgr, scope, cfg, namespace, mesh, vip, port)
}

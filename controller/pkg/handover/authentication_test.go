package handover

import (
	"context"
	"fmt"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestReceiptAuthenticationRejectsIncompleteReviews(t *testing.T) {
	const bearer = "credential-must-not-appear-in-errors"
	for _, fault := range []string{"api-error", "status-error", "unauthenticated", "missing-audience", "wrong-audience", "missing-pod", "missing-node", "ambiguous-node", "wrong-account"} {
		t.Run(fault, func(t *testing.T) {
			api := fake.NewSimpleClientset()
			verifier := PodReceiptAuthenticator{API: api, Audience: "receipts", Namespace: "test", ServiceAccountName: "dialer", ServiceAccountUID: "sa-uid", DaemonSetName: "dialer", DaemonSetUID: "daemon-uid"}
			status := authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{verifier.Audience}, User: authenticationv1.UserInfo{Username: "system:serviceaccount:test:dialer", UID: "sa-uid", Extra: map[string]authenticationv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {"pod"}, "authentication.kubernetes.io/pod-uid": {"pod-uid"}, "authentication.kubernetes.io/node-name": {"node"}, "authentication.kubernetes.io/node-uid": {"node-uid"},
			}}}
			switch fault {
			case "status-error":
				status.Error = bearer
			case "unauthenticated":
				status.Authenticated = false
			case "missing-audience":
				status.Audiences = nil
			case "wrong-audience":
				status.Audiences = []string{"different"}
			case "missing-pod":
				delete(status.User.Extra, "authentication.kubernetes.io/pod-uid")
			case "missing-node":
				delete(status.User.Extra, "authentication.kubernetes.io/node-uid")
			case "ambiguous-node":
				status.User.Extra["authentication.kubernetes.io/node-uid"] = authenticationv1.ExtraValue{"node-uid", "other"}
			case "wrong-account":
				status.User.Username = "system:serviceaccount:other:dialer"
			}
			api.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
				request := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
				if request.Spec.Token != bearer || len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != verifier.Audience {
					t.Fatal("review did not bind the explicit audience and credential")
				}
				if fault == "api-error" {
					return true, nil, fmt.Errorf("upstream echoed %s", bearer)
				}
				return true, &authenticationv1.TokenReview{Status: status}, nil
			})
			proof, err := verifier.Authenticate(context.Background(), bearer, Receipt{NodeUID: "node-uid"}, "boot")
			if err == nil || proof != (AuthenticatedReceipt{}) {
				t.Fatal("incomplete authentication accepted")
			}
			if strings.Contains(err.Error(), bearer) {
				t.Fatal("credential leaked through error")
			}
			if len(api.Actions()) != 1 {
				t.Fatal("invalid identity reached object authorization")
			}
		})
	}
}

/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/ingress-gce/pkg/annotations"
	backendconfig "k8s.io/ingress-gce/pkg/apis/backendconfig/v1beta1"
	"k8s.io/ingress-gce/pkg/e2e"
	"k8s.io/ingress-gce/pkg/fuzz"
	"k8s.io/ingress-gce/pkg/fuzz/features"
	"k8s.io/ingress-gce/pkg/utils"
)

func TestDraining(t *testing.T) {
	t.Parallel()

	ing := fuzz.NewIngressBuilder("", "ingress-1", "").
		AddPath("test.com", "/", "service-1", intstr.FromInt(80)).
		Build()

	for _, tc := range []struct {
		desc     string
		beConfig *backendconfig.BackendConfig
	}{
		{
			desc: "http with 60s draining timeout",
			beConfig: fuzz.NewBackendConfigBuilder("", "backendconfig-1").
				SetConnectionDrainingTimeout(60).
				Build(),
		},
		{
			desc: "http no draining defined",
			beConfig: fuzz.NewBackendConfigBuilder("", "backendconfig-1").
				Build(),
		},
	} {
		tc := tc // Capture tc as we are running this in parallel.
		Framework.RunWithSandbox(tc.desc, t, func(t *testing.T, s *e2e.Sandbox) {
			t.Parallel()

			ctx := context.Background()

			backendConfigAnnotation := map[string]string{
				annotations.BackendConfigKey: `{"default":"backendconfig-1"}`,
			}

			if _, err := Framework.BackendConfigClient.CloudV1beta1().BackendConfigs(s.Namespace).Create(tc.beConfig); err != nil {
				t.Fatalf("error creating BackendConfig: %v", err)
			}
			t.Logf("BackendConfig created (%s/%s) ", s.Namespace, tc.beConfig.Name)

			_, _, err := e2e.CreateEchoService(s, "service-1", backendConfigAnnotation)
			if err != nil {
				t.Fatalf("error creating echo service: %v", err)
			}
			t.Logf("Echo service created (%s/%s)", s.Namespace, "service-1")

			if _, err := Framework.Clientset.Extensions().Ingresses(s.Namespace).Create(ing); err != nil {
				t.Fatalf("error creating Ingress spec: %v", err)
			}
			t.Logf("Ingress created (%s/%s)", s.Namespace, ing.Name)

			ing, err := e2e.WaitForIngress(s, ing)
			if err != nil {
				t.Fatalf("error waiting for Ingress to stabilize: %v", err)
			}
			t.Logf("GCLB resources created (%s/%s)", s.Namespace, ing.Name)

			vip := ing.Status.LoadBalancer.Ingress[0].IP
			t.Logf("Ingress %s/%s VIP = %s", s.Namespace, ing.Name, vip)
			gclb, err := fuzz.GCLBForVIP(context.Background(), Framework.Cloud, vip, fuzz.FeatureValidators(features.All))
			if err != nil {
				t.Fatalf("Error getting GCP resources for LB with IP = %q: %v", vip, err)
			}

			timeout := int64(0)
			if tc.beConfig.Spec.ConnectionDraining != nil {
				timeout = tc.beConfig.Spec.ConnectionDraining.DrainingTimeoutSec
			}

			if err := verifyConnectionDrainingTimeout(t, gclb, s.Namespace, "service-1", timeout); err != nil {
				t.Errorf("verifyConnectionDrainingTimeout(..., %q, %q, %d) = %v, want nil", s.Namespace, "service-1", timeout, err)
			}

			// Wait for GCLB resources to be deleted.
			deleteOptions := &fuzz.GCLBDeleteOptions{
				SkipDefaultBackend: true,
			}
			if err := Framework.Clientset.Extensions().Ingresses(s.Namespace).Delete(ing.Name, &metav1.DeleteOptions{}); err != nil {
				t.Errorf("Delete(%q) = %v, want nil", ing.Name, err)
			}
			t.Logf("Waiting for GCLB resources to be deleted (%s/%s)", s.Namespace, ing.Name)
			if err := e2e.WaitForGCLBDeletion(ctx, Framework.Cloud, gclb, deleteOptions); err != nil {
				t.Errorf("e2e.WaitForGCLBDeletion(...) = %v, want nil", err)
			}
			t.Logf("GCLB resources deleted (%s/%s)", s.Namespace, ing.Name)
		})
	}
}

func verifyConnectionDrainingTimeout(t *testing.T, gclb *fuzz.GCLB, svcNamespace, svcName string, expectedTimeout int64) error {
	for _, bs := range gclb.BackendService {
		desc := utils.DescriptionFromString(bs.GA.Description)
		if desc.ServiceName != fmt.Sprintf("%s/%s", svcNamespace, svcName) {
			continue
		}
		if bs.GA.ConnectionDraining == nil {
			return fmt.Errorf("backend service %q has no draining configuration", bs.GA.Name)
		}
		if bs.GA.ConnectionDraining.DrainingTimeoutSec != expectedTimeout {
			return fmt.Errorf("backend service %q has draining timeout %d, want %d", bs.GA.Name,
				bs.GA.ConnectionDraining.DrainingTimeoutSec, expectedTimeout)
		}
	}
	return nil
}

//go:build integ
// +build integ

// Copyright Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// While most of this suite is a copy of the Gateway API-focused tests in
// tests/integration/pilot/ingress_test.go, it performs these tests with
// maistra multi-tenancy enabled and adds a test case for namespace-selectors,
// which are not supported in maistra. Usage of namespace selectors in a
// Gateway resource will be ignored and interpreted like the default case,
// ie only Routes from the same namespace will be taken into account for
// that listener.

package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sv1 "sigs.k8s.io/gateway-api/apis/v1"

	"istio.io/istio/pilot/pkg/model/kstatus"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/http/headers"
	"istio.io/istio/pkg/test/echo/common/scheme"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/framework/components/echo/match"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/namespace"
	testKube "istio.io/istio/pkg/test/kube"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
	ingressutil "istio.io/istio/tests/integration/security/sds_ingress/util"
	"istio.io/istio/tests/integration/servicemesh/maistra"
)

const customGatewayClassAndControllerPatch = `[
	{
		"op": "add",
		"path": "/spec/template/spec/containers/0/env/-",
		"value": {
			"name": "PILOT_GATEWAY_API_DEFAULT_GATEWAYCLASS",
			"value": "openshift-default"
		}
	},
	{
		"op": "add",
		"path": "/spec/template/spec/containers/0/env/-",
		"value": {
			"name": "PILOT_GATEWAY_API_CONTROLLER_NAME",
			"value": "openshift.io/gateway-controller"
		}
	}
]`

var (
	istioNs     namespace.Instance
	secondaryNs namespace.Instance
	appNs       namespace.Instance
	apps        echo.Instances
	appsMux     sync.Mutex
)

func TestMain(m *testing.M) {
	// nolint: staticcheck
	framework.
		NewSuite(m).
		RequireMaxClusters(1).
		SetupParallel(maistra.ApplyServiceMeshCRDs, maistra.ApplyGatewayAPICRDs).
		SetupParallel(
			namespace.Setup(&istioNs, namespace.Config{Prefix: "istio-system"}),
			namespace.Setup(&appNs, namespace.Config{Prefix: "app"}),
			namespace.Setup(&secondaryNs, namespace.Config{Prefix: "secondary", Labels: map[string]string{"test": "test"}})).
		Setup(maistra.Install(namespace.Future(&istioNs), &maistra.InstallationOptions{EnableGatewayAPI: true, OutboundAllowAny: true})).
		Setup(maistra.RemoveDefaultRBAC).
		Setup(maistra.ApplyRestrictedRBAC(namespace.Future(&istioNs))).
		Setup(maistra.DisableWebhooksAndRestart(namespace.Future(&istioNs))).
		Run()
}

func TestGateway(t *testing.T) {
	framework.
		NewTest(t).
		Run(func(t framework.TestContext) {
			if err := maistra.ApplyServiceMeshMemberRoll(t, istioNs, appNs.Name()); err != nil {
				t.Errorf("failed to apply SMMR for namespace %s: %s", appNs.Name(), err)
			}

			if err := maistra.DeployEchos(&apps, &appsMux, "a", namespace.Future(&appNs), maistra.AppOpts{Revision: istioNs.Prefix()})(t); err != nil {
				t.Errorf("failed to deploy app 'a': %s", err)
			}
			if err := maistra.DeployEchos(&apps, &appsMux, "b", namespace.Future(&appNs), maistra.AppOpts{Revision: istioNs.Prefix()})(t); err != nil {
				t.Errorf("failed to deploy app 'b': %s", err)
			}

			t.NewSubTest("unmanaged").Run(func(t framework.TestContext) {
				UnmanagedGatewayTest(t, "istio")
			})
			t.NewSubTest("managed").Run(func(t framework.TestContext) {
				ManagedGatewayTest(t, "istio")
			})
			t.NewSubTest("managed-owner").Run(func(t framework.TestContext) {
				ManagedOwnerGatewayTest(t, "istio")
			})
			t.NewSubTest("managed-short-name").Run(func(t framework.TestContext) {
				ManagedGatewayShortNameTest(t, "istio")
			})

			patchFn := maistra.PatchIstiodAndRestart(namespace.Future(&istioNs), customGatewayClassAndControllerPatch)
			if err := patchFn(t); err != nil {
				t.Errorf("failed to patch istiod deployment: %s", err)
			}

			t.NewSubTest("unmanaged-custom-names").Run(func(t framework.TestContext) {
				UnmanagedGatewayTest(t, "openshift-default")
			})
			t.NewSubTest("managed-custom-names").Run(func(t framework.TestContext) {
				ManagedGatewayTest(t, "openshift-default")
			})
			t.NewSubTest("managed-owner-custom-names").Run(func(t framework.TestContext) {
				ManagedOwnerGatewayTest(t, "openshift-default")
			})
			t.NewSubTest("managed-short-name-custom-names").Run(func(t framework.TestContext) {
				ManagedGatewayShortNameTest(t, "openshift-default")
			})
		})
}

func ManagedOwnerGatewayTest(t framework.TestContext, gatewayClassName string) {
	image := fmt.Sprintf("%s/app:%s", t.Settings().Image.Hub, strings.TrimSuffix(t.Settings().Image.Tag, "-distroless"))
	t.ConfigIstio().YAML(appNs.Name(), fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata:
  name: managed-owner-istio
spec:
  ports:
  - appProtocol: http
    name: default
    port: 80
  selector:
    istio.io/gateway-name: managed-owner
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: managed-owner-istio
spec:
  selector:
    matchLabels:
      istio.io/gateway-name: managed-owner
  replicas: 1
  template:
    metadata:
      labels:
        istio.io/gateway-name: managed-owner
    spec:
      containers:
      - name: fake
        image: %s
`, image)).ApplyOrFail(t)
	cls := t.Clusters().Kube().Default()
	fetchFn := testKube.NewSinglePodFetch(cls, appNs.Name(), "istio.io/gateway-name=managed-owner")
	if _, err := testKube.WaitUntilPodsAreReady(fetchFn); err != nil {
		t.Fatal(err)
	}

	t.ConfigIstio().YAML(appNs.Name(), fmt.Sprintf(`
apiVersion: gateway.networking.k8s.io/v1beta1
kind: Gateway
metadata:
  name: managed-owner
spec:
  gatewayClassName: %s
  listeners:
  - name: default
    hostname: "*.example.com"
    port: 80
    protocol: HTTP
`, gatewayClassName)).ApplyOrFail(t)

	// Make sure Gateway becomes programmed..
	client := t.Clusters().Kube().Default().GatewayAPI().GatewayV1beta1().Gateways(appNs.Name())
	check := func() error {
		gw, _ := client.Get(context.Background(), "managed-owner", metav1.GetOptions{})
		if gw == nil {
			return fmt.Errorf("failed to find gateway")
		}
		cond := kstatus.GetCondition(gw.Status.Conditions, string(k8sv1.GatewayConditionProgrammed))
		if cond.Status != metav1.ConditionTrue {
			return fmt.Errorf("failed to find programmed condition: %+v", cond)
		}
		if cond.ObservedGeneration != gw.Generation {
			return fmt.Errorf("stale GWC generation: %+v", cond)
		}
		return nil
	}
	retry.UntilSuccessOrFail(t, check)

	// Make sure we did not overwrite our deployment or service
	dep, err := t.Clusters().Kube().Default().Kube().AppsV1().Deployments(appNs.Name()).
		Get(context.Background(), "managed-owner-istio", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, dep.Labels[constants.ManagedGatewayLabel], "")
	assert.Equal(t, dep.Spec.Template.Spec.Containers[0].Image, image)

	svc, err := t.Clusters().Kube().Default().Kube().CoreV1().Services(appNs.Name()).
		Get(context.Background(), "managed-owner-istio", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, svc.Labels[constants.ManagedGatewayLabel], "")
	assert.Equal(t, svc.Spec.Type, corev1.ServiceTypeClusterIP)
}

func ManagedGatewayTest(t framework.TestContext, gatewayClassName string) {
	t.ConfigIstio().YAML(appNs.Name(), fmt.Sprintf(`
apiVersion: gateway.networking.k8s.io/v1beta1
kind: Gateway
metadata:
  name: gateway
spec:
  gatewayClassName: %s
  listeners:
  - name: default
    hostname: "*.example.com"
    port: 80
    protocol: HTTP
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: http-1
spec:
  parentRefs:
  - name: gateway
  hostnames: ["bar.example.com"]
  rules:
  - backendRefs:
    - name: b
      port: 80
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: http-2
spec:
  parentRefs:
  - name: gateway
  hostnames: ["foo.example.com"]
  rules:
  - backendRefs:
    - name: d
      port: 80
`, gatewayClassName)).ApplyOrFail(t)
	testCases := []struct {
		check echo.Checker
		from  echo.Instances
		host  string
	}{
		{
			check: check.OK(),
			from:  echo.Instances{apps[1]},
			host:  "bar.example.com",
		},
		{
			check: check.NotOK(),
			from:  echo.Instances{apps[1]},
			host:  "bar",
		},
	}
	for _, tc := range testCases {
		t.NewSubTest(fmt.Sprintf("gateway-connectivity-from-%s", tc.from[0].NamespacedName())).Run(func(t framework.TestContext) {
			tc.from[0].CallOrFail(t, echo.CallOptions{
				Port: echo.Port{
					Protocol:    protocol.HTTP,
					ServicePort: 80,
				},
				Scheme: scheme.HTTP,
				HTTP: echo.HTTP{
					Headers: headers.New().WithHost(tc.host).Build(),
				},
				Address: fmt.Sprintf("gateway-%s.%s.svc.cluster.local", gatewayClassName, appNs.Name()),
				Check:   tc.check,
			})
		})
	}
}

func ManagedGatewayShortNameTest(t framework.TestContext, gatewayClassName string) {
	t.ConfigIstio().YAML(appNs.Name(), fmt.Sprintf(`
apiVersion: gateway.networking.k8s.io/v1beta1
kind: Gateway
metadata:
  name: gateway
spec:
  gatewayClassName: %s
  listeners:
  - name: default
    hostname: "bar"
    port: 80
    protocol: HTTP
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: http
spec:
  parentRefs:
  - name: gateway
  rules:
  - backendRefs:
    - name: b
      port: 80
`, gatewayClassName)).ApplyOrFail(t)
	a := match.ServiceName(echo.NamespacedName{Name: "a", Namespace: appNs}).GetMatches(apps).Instances()[0]
	a.CallOrFail(t, echo.CallOptions{
		Port:   echo.Port{ServicePort: 80},
		Scheme: scheme.HTTP,
		HTTP: echo.HTTP{
			Headers: headers.New().WithHost("bar").Build(),
		},
		Address: fmt.Sprintf("gateway-%s.%s.svc.cluster.local", gatewayClassName, appNs.Name()),
		Check:   check.OK(),
		Retry: echo.Retry{
			Options: []retry.Option{retry.Timeout(time.Minute)},
		},
	})
	apps[1].CallOrFail(t, echo.CallOptions{
		Port:   echo.Port{ServicePort: 80},
		Scheme: scheme.HTTP,
		HTTP: echo.HTTP{
			Headers: headers.New().WithHost("bar.example.com").Build(),
		},
		Address: fmt.Sprintf("gateway-istio.%s.svc.cluster.local", appNs.Name()),
		Check:   check.NotOK(),
		Retry: echo.Retry{
			Options: []retry.Option{retry.Timeout(time.Minute)},
		},
	})
}

func UnmanagedGatewayTest(t framework.TestContext, gatewayClassName string) {
	ingressutil.CreateIngressKubeSecret(t, "test-gateway-cert-same", ingressutil.TLS, ingressutil.IngressCredentialA,
		false, t.Clusters().Configs()...)
	ingressutil.CreateIngressKubeSecretInNamespace(t, "test-gateway-cert-cross", ingressutil.TLS, ingressutil.IngressCredentialB,
		false, appNs.Name(), t.Clusters().Configs()...)

	t.ConfigIstio().
		YAML(istioNs.Name(), fmt.Sprintf(`
apiVersion: gateway.networking.k8s.io/v1beta1
kind: Gateway
metadata:
  name: gateway
spec:
  addresses:
  - value: istio-ingressgateway
    type: Hostname
  gatewayClassName: %s
  listeners:
  - name: http
    hostname: "*.domain.example"
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: All
  - name: http-secondary
    hostname: "secondary.namespace"
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        selector:
          matchLabels:
            test: test
  - name: tcp
    port: 31400
    protocol: TCP
    allowedRoutes:
      namespaces:
        from: All
  - name: tls-cross
    hostname: cross-namespace.domain.example
    port: 443
    protocol: HTTPS
    allowedRoutes:
      namespaces:
        from: All
    tls:
      mode: Terminate
      certificateRefs:
      - kind: Secret
        name: test-gateway-cert-cross
        namespace: "%s"
  - name: tls-same
    hostname: same-namespace.domain.example
    port: 443
    protocol: HTTPS
    allowedRoutes:
      namespaces:
        from: All
    tls:
      mode: Terminate
      certificateRefs:
      - kind: Secret
        name: test-gateway-cert-same
`, gatewayClassName, appNs.Name())).
		YAML(appNs.Name(), fmt.Sprintf(`
apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: http
spec:
  hostnames: ["my.domain.example"]
  parentRefs:
  - name: gateway
    namespace: %[1]s
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /get/
    backendRefs:
    - name: b
      port: 80
---
apiVersion: gateway.networking.k8s.io/v1alpha2
kind: TCPRoute
metadata:
  name: tcp
spec:
  parentRefs:
  - name: gateway
    namespace: %[1]s
  rules:
  - backendRefs:
    - name: b
      port: 80
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: b
spec:
  parentRefs:
  - group: ""
    kind: Service
    name: b
  - name: gateway
    namespace: %[1]s
  hostnames: ["b"]
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /path
    filters:
    - type: RequestHeaderModifier
      requestHeaderModifier:
        add:
        - name: my-added-header
          value: added-value
    backendRefs:
    - name: b
      port: 80
`, istioNs.Name())).
		YAML(secondaryNs.Name(), fmt.Sprintf(`
apiVersion: gateway.networking.k8s.io/v1beta1
kind: HTTPRoute
metadata:
  name: http
spec:
  hostnames: ["secondary.namespace"]
  parentRefs:
  - name: gateway
    namespace: %s
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /get/
    backendRefs:
    - name: b
      namespace: %s
      port: 80
`, istioNs.Name(), appNs.Name())).
		ApplyOrFail(t)
	for _, ingr := range istio.IngressesOrFail(t, t) {
		t.NewSubTest(ingr.Cluster().StableName()).Run(func(t framework.TestContext) {
			t.NewSubTest("http").Run(func(t framework.TestContext) {
				paths := []string{"/get", "/get/", "/get/prefix"}
				for _, path := range paths {
					_ = ingr.CallOrFail(t, echo.CallOptions{
						Port: echo.Port{
							Protocol: protocol.HTTP,
						},
						HTTP: echo.HTTP{
							Path:    path,
							Headers: headers.New().WithHost("my.domain.example").Build(),
						},
						Check: check.OK(),
					})
				}
			})
			t.NewSubTest("http-non-mesh-namespace").Run(func(t framework.TestContext) {
				paths := []string{"/get", "/get/", "/get/prefix"}
				for _, path := range paths {
					_ = ingr.CallOrFail(t, echo.CallOptions{
						Port: echo.Port{
							Protocol: protocol.HTTP,
						},
						HTTP: echo.HTTP{
							Path: path,
							Headers: http.Header{
								"Host": {"secondary.namespace"},
							},
						},
						Check: check.NoErrorAndStatus(404),
					})
				}
			})
			t.NewSubTest("tcp").Run(func(t framework.TestContext) {
				_ = ingr.CallOrFail(t, echo.CallOptions{
					Port: echo.Port{
						Protocol:    protocol.HTTP,
						ServicePort: 31400,
					},
					HTTP: echo.HTTP{
						Path:    "/",
						Headers: headers.New().WithHost("my.domain.example").Build(),
					},
					Check: check.OK(),
				})
			})
			t.NewSubTest("mesh").Run(func(t framework.TestContext) {
				a := match.ServiceName(echo.NamespacedName{Name: "a", Namespace: appNs}).GetMatches(apps).Instances()[0]
				b := match.ServiceName(echo.NamespacedName{Name: "b", Namespace: appNs}).GetMatches(apps).Instances()[0]
				_ = a.CallOrFail(t, echo.CallOptions{
					To:    b,
					Count: 1,
					Port: echo.Port{
						Name: "http",
					},
					HTTP: echo.HTTP{
						Path: "/path",
					},
					Check: check.And(
						check.OK(),
						check.RequestHeader("My-Added-Header", "added-value")),
				})
			})
			t.NewSubTest("status").Run(func(t framework.TestContext) {
				retry.UntilSuccessOrFail(t, func() error {
					gw, err := t.Clusters().Kube().Default().GatewayAPI().GatewayV1beta1().Gateways(istioNs.Name()).
						Get(context.Background(), "gateway", metav1.GetOptions{})
					if err != nil {
						return err
					}
					cond := kstatus.GetCondition(gw.Status.Conditions, string(k8sv1.GatewayConditionProgrammed))
					if cond.Status != metav1.ConditionTrue {
						return fmt.Errorf("failed to find programmed condition: %+v", cond)
					}
					if cond.ObservedGeneration != gw.Generation {
						return fmt.Errorf("stale GW generation: %+v", cond)
					}
					return nil
				})
			})
		})
	}
}

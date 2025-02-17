// Copyright Istio Authors
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

package install

import (
	"os"
	"path/filepath"
	"testing"

	"istio.io/istio/cni/pkg/config"
	"istio.io/istio/cni/pkg/constants"
	testutils "istio.io/istio/pilot/test/util"
)

const (
	k8sServiceHost = "10.96.0.1"
	k8sServicePort = "443"
	kubeCAFilepath = "testdata/kube-ca.crt"
	saToken        = "service_account_token_string"
)

func TestCreateValidKubeconfigFile(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "token"), []byte(saToken), 0o644)
	constants.ServiceAccountPath = tmp
	cases := []struct {
		name               string
		expectedFailure    bool
		k8sServiceProtocol string
		k8sServiceHost     string
		k8sServicePort     string
		kubeCAFilepath     string
		skipTLSVerify      bool
		cniNetDir          string
	}{
		{
			name:            "k8s service host not set",
			expectedFailure: true,
		},
		{
			name:            "k8s service port not set",
			expectedFailure: true,
			k8sServiceHost:  k8sServiceHost,
		},
		{
			name:           "skip TLS verify",
			k8sServiceHost: k8sServiceHost,
			k8sServicePort: k8sServicePort,
			skipTLSVerify:  true,
		},
		{
			name:           "TLS verify",
			k8sServiceHost: k8sServiceHost,
			k8sServicePort: k8sServicePort,
			kubeCAFilepath: kubeCAFilepath,
		},
		{
			name:           "nonexistent net.d dir",
			k8sServiceHost: k8sServiceHost,
			k8sServicePort: k8sServicePort,
			skipTLSVerify:  true,
			cniNetDir:      filepath.Join(t.TempDir(), "nonexistent-dir"),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tempDir := c.cniNetDir
			// Create temp directory for files if not provided in test case
			if tempDir == "" {
				tempDir = t.TempDir()
			}

			cfg := &config.InstallConfig{
				MountedCNINetDir:   tempDir,
				KubeCAFile:         c.kubeCAFilepath,
				K8sServiceProtocol: c.k8sServiceProtocol,
				K8sServiceHost:     c.k8sServiceHost,
				K8sServicePort:     c.k8sServicePort,
				SkipTLSVerify:      c.skipTLSVerify,
			}
			result, err := createKubeConfig(cfg)
			if err != nil {
				if !c.expectedFailure {
					t.Fatalf("did not expect failure: %v", err)
				}
				// Successful test case expecting failure
				return
			} else if c.expectedFailure {
				t.Fatalf("expected failure")
			}

			goldenFilepath := "testdata/kubeconfig-tls"
			if c.skipTLSVerify {
				goldenFilepath = "testdata/kubeconfig-skip-tls"
			}

			testutils.CompareContent(t, []byte(result.Full), goldenFilepath)
		})
	}
}

func TestReplaceInvalidKubeconfigFile(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "token"), []byte(saToken), 0o644)
	constants.ServiceAccountPath = tmp
	tempDir := t.TempDir()

	cfg := &config.InstallConfig{
		MountedCNINetDir: tempDir,
		KubeCAFile:       kubeCAFilepath,
		K8sServiceHost:   k8sServiceHost,
		K8sServicePort:   k8sServicePort,
	}
	// Write out a kubeconfig with one cert
	result, err := createKubeConfig(cfg)
	if err != nil {
		t.Fatalf("did not expect failure: %v", err)
	}
	goldenFilepath := "testdata/kubeconfig-tls"
	testutils.CompareContent(t, []byte(result.Full), goldenFilepath)

	newk8sServiceHost := "50.76.2.1"
	newCfg := &config.InstallConfig{
		MountedCNINetDir: tempDir,
		KubeCAFile:       kubeCAFilepath,
		K8sServiceHost:   newk8sServiceHost,
		K8sServicePort:   k8sServicePort,
	}
	// Write out a kubeconfig with one cert
	result, err = createKubeConfig(newCfg)
	if err != nil {
		t.Fatalf("did not expect failure: %v", err)
	}
	goldenNewFilepath := "testdata/kubeconfig-newhost"
	testutils.CompareContent(t, []byte(result.Full), goldenNewFilepath)
}

func TestCheckNoExistingKubeConfig(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "token"), []byte(saToken), 0o644)
	constants.ServiceAccountPath = tmp
	tempDir := t.TempDir()

	cfg := &config.InstallConfig{
		MountedCNINetDir: tempDir,
		KubeCAFile:       kubeCAFilepath,
		K8sServiceHost:   k8sServiceHost,
		K8sServicePort:   k8sServicePort,
	}

	expectedKC, err := createKubeConfig(cfg)
	if err != nil {
		t.Fatalf("expected no error: %+v", err)
	}
	err = checkExistingKubeConfigFile(cfg, expectedKC)

	if err == nil {
		t.Fatalf("expected error, no kubeconfig present")
	}
}

func TestCheckMismatchedExistingKubeConfig(t *testing.T) {
	tmp := t.TempDir()
	os.WriteFile(filepath.Join(tmp, "token"), []byte(saToken), 0o644)
	constants.ServiceAccountPath = tmp
	tempDir := t.TempDir()

	cfg := &config.InstallConfig{
		MountedCNINetDir:   tempDir,
		KubeCAFile:         kubeCAFilepath,
		K8sServiceHost:     k8sServiceHost,
		K8sServicePort:     k8sServicePort,
		KubeconfigFilename: "dork.cfg",
	}

	expectedKC, err := createKubeConfig(cfg)
	if err != nil {
		t.Fatalf("expected no error: %+v", err)
	}
	os.WriteFile(filepath.Join(cfg.MountedCNINetDir, cfg.KubeconfigFilename), []byte(expectedKC.Full), 0o644)

	err = checkExistingKubeConfigFile(cfg, expectedKC)

	if err != nil {
		t.Fatalf("expected no error, matching kubeconfig present, got %+v", err)
	}
}

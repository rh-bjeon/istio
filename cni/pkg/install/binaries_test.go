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
	"path/filepath"
	"testing"

	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/file"
)

func TestCopyBinaries(t *testing.T) {
	cases := []struct {
		name          string
		srcFiles      map[string]string
		existingFiles map[string]string
		expectedFiles map[string]string
		prefix        string
	}{
		{
			name:          "basic",
			srcFiles:      map[string]string{"istio-cni": "cni111", "istio-iptables": "iptables111"},
			expectedFiles: map[string]string{"istio-cni": "cni111", "istio-iptables": "iptables111"},
		},
		{
			name:          "update binaries",
			srcFiles:      map[string]string{"istio-cni": "cni111", "istio-iptables": "iptables111"},
			existingFiles: map[string]string{"istio-cni": "cni000", "istio-iptables": "iptables111"},
			expectedFiles: map[string]string{"istio-cni": "cni111", "istio-iptables": "iptables111"},
		},
		{
			name:          "binaries prefix",
			prefix:        "prefix-",
			srcFiles:      map[string]string{"istio-cni": "cni111", "istio-iptables": "iptables111"},
			expectedFiles: map[string]string{"prefix-istio-cni": "cni111", "prefix-istio-iptables": "iptables111"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srcDir := t.TempDir()
			for filename, contents := range c.srcFiles {
				file.WriteOrFail(t, filepath.Join(srcDir, filename), []byte(contents))
			}

			targetDir := t.TempDir()
			for filename, contents := range c.existingFiles {
				file.WriteOrFail(t, filepath.Join(targetDir, filename), []byte(contents))
			}

			binariesCopied, err := copyBinaries(srcDir, []string{targetDir}, c.prefix)
			if err != nil {
				t.Fatal(err)
			}

			for filename, expectedContents := range c.expectedFiles {
				contents := file.AsStringOrFail(t, filepath.Join(targetDir, filename))
				assert.Equal(t, contents, expectedContents)

				wasCopied := false
				for _, bin := range binariesCopied.UnsortedList() {
					if bin == filename {
						wasCopied = true
					}
				}
				assert.Equal(t, wasCopied, true)
			}
		})
	}
}

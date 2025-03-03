/*
Copyright 2019 The Vitess Authors.

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

package env

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVtDataRoot(t *testing.T) {
	envVar := "VTDATAROOT"
	oldEnvVar := os.Getenv(envVar)

	if oldEnvVar != "" {
		os.Setenv(envVar, "")
	}

	defer os.Setenv(envVar, oldEnvVar)

	root := VtDataRoot()
	if root != DefaultVtDataRoot {
		t.Errorf("When VTDATAROOT is not set, the default value should be %v, not %v.", DefaultVtDataRoot, root)
	}

	passed := "/tmp"
	os.Setenv(envVar, passed)
	root = VtDataRoot()
	if root != passed {
		t.Errorf("The value of VtDataRoot should be %v, not %v.", passed, root)
	}
}

func TestPlannerVersion(t *testing.T) {
	empty := ""
	v3 := "V3"
	gen4 := "gen4"

	tests := []struct {
		a, b   *string
		expect string
		err    bool
	}{{
		a:   &v3,
		b:   &gen4,
		err: true,
	}, {
		a:      &v3,
		b:      &v3,
		expect: v3,
	}, {
		a:      &v3,
		b:      nil,
		expect: v3,
	}, {
		a:      nil,
		b:      &gen4,
		expect: gen4,
	}, {
		a:      &v3,
		b:      &empty,
		expect: v3,
	}, {
		a:      &empty,
		b:      &v3,
		expect: v3,
	}}

	for i, test := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			v, err := CheckPlannerVersionFlag(test.a, test.b)
			if test.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.expect, v)
		})
	}
}

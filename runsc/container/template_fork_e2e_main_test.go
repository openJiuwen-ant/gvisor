// Copyright 2026 The gVisor Authors.
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

package container

import (
	"fmt"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/test/testutil"
	"gvisor.dev/gvisor/runsc/config"
	runscflag "gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/specutils"
)

func TestMain(m *testing.M) {
	config.RegisterFlags(runscflag.CommandLine)
	runscflag.Parse()
	if err := testutil.ConfigureExePath(); err != nil {
		panic(err.Error())
	}
	if err := specutils.MaybeRunAsRoot(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running as root: %v\n", err)
		os.Exit(123)
	}
	os.Exit(runTemplateForkTestsInNetNS(m, templateForkE2ENetNSEnv))
}

/*
Copyright 2026 The Aenix Authors.

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

package enginetest_test

import (
	"testing"

	"github.com/aenix-io/site-router-operator/api/engine/enginetest"
	"github.com/aenix-io/site-router-operator/api/engine/enginetest/cmdfake"
	"github.com/aenix-io/site-router-operator/api/engine/enginetest/filesfake"
)

func TestConformanceCmdFake(t *testing.T) {
	enginetest.Run(t, func(*testing.T) enginetest.Rig {
		dev := cmdfake.NewDevice()
		return enginetest.Rig{Engine: cmdfake.Engine{Dev: dev}, Dial: dev.Dial, BreakFilter: dev.BreakFilter}
	})
}

func TestConformanceFilesFake(t *testing.T) {
	enginetest.Run(t, func(*testing.T) enginetest.Rig {
		dev := filesfake.NewDevice()
		return enginetest.Rig{Engine: filesfake.Engine{Dev: dev}, Dial: dev.Dial, BreakFilter: dev.BreakFilter}
	})
}

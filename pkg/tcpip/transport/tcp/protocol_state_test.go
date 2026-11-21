// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tcp

import (
	"bytes"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

func TestResetRandomState(t *testing.T) {
	s := stack.New(stack.Options{
		TransportProtocols: []stack.TransportProtocolFactory{NewProtocol},
		SecureRNG:          bytes.NewReader(make([]byte, 64)),
	})
	defer s.Destroy()
	p := protocolFromStack(s)
	oldSeqnumSecret := p.seqnumSecret
	oldTSOffsetSecret := p.tsOffsetSecret
	oldDispatcherSeed := p.dispatcher.hasher.seed

	s.Pause()
	s.ResetRandomState(bytes.NewReader(bytes.Repeat([]byte{0xff}, 64)))
	s.Resume()

	if p.seqnumSecret == oldSeqnumSecret {
		t.Errorf("seqnumSecret was not reset")
	}
	if p.tsOffsetSecret == oldTSOffsetSecret {
		t.Errorf("tsOffsetSecret was not reset")
	}
	if p.dispatcher.hasher.seed == oldDispatcherSeed {
		t.Errorf("dispatcher hash seed was not reset")
	}
}

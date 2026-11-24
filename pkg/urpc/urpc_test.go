// Copyright 2018 The gVisor Authors.
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

package urpc

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/unet"
)

type test struct {
}

type testArg struct {
	StringArg string
	IntArg    int
	FilePayload
}

type testResult struct {
	StringResult string
	IntResult    int
	FilePayload
}

func (t test) Func(a *testArg, r *testResult) error {
	r.StringResult = a.StringArg
	r.IntResult = a.IntArg
	return nil
}

var errTest = errors.New("test error")

func (t test) Err(a *testArg, r *testResult) error {
	return errTest
}

func (t test) FailNoFile(a *testArg, r *testResult) error {
	if a.Files == nil {
		return errors.New("no file found")
	}

	return nil
}

func (t test) SendFile(a *testArg, r *testResult) error {
	r.Files = []*os.File{os.Stdin, os.Stdout, os.Stderr}
	return nil
}

func (t test) TooManyFiles(a *testArg, r *testResult) error {
	for i := 0; i <= maxFiles; i++ {
		r.Files = append(r.Files, os.Stdin)
	}
	return nil
}

func startServer(socket *unet.Socket) {
	s := NewServer()
	s.Register(test{})
	s.StartHandling(socket)
}

func testClient() (*Client, error) {
	serverSock, clientSock, err := unet.SocketPair(false)
	if err != nil {
		return nil, err
	}
	startServer(serverSock)

	return NewClient(clientSock), nil
}

func TestCall(t *testing.T) {
	c, err := testClient()
	if err != nil {
		t.Fatalf("error creating test client: %v", err)
	}
	defer c.Close()

	var r testResult
	if err := c.Call("test.Func", &testArg{}, &r); err != nil {
		t.Errorf("basic call failed: %v", err)
	} else if r.StringResult != "" || r.IntResult != 0 {
		t.Errorf("unexpected result, got %v expected zero value", r)
	}
	if err := c.Call("test.Func", &testArg{StringArg: "hello"}, &r); err != nil {
		t.Errorf("basic call failed: %v", err)
	} else if r.StringResult != "hello" {
		t.Errorf("unexpected result, got %v expected hello", r.StringResult)
	}
	if err := c.Call("test.Func", &testArg{IntArg: 1}, &r); err != nil {
		t.Errorf("basic call failed: %v", err)
	} else if r.IntResult != 1 {
		t.Errorf("unexpected result, got %v expected 1", r.IntResult)
	}
}

func TestUnknownMethod(t *testing.T) {
	c, err := testClient()
	if err != nil {
		t.Fatalf("error creating test client: %v", err)
	}
	defer c.Close()

	var r testResult
	if err := c.Call("test.Unknown", &testArg{}, &r); err == nil {
		t.Errorf("expected non-nil err, got nil")
	} else if err.Error() != ErrUnknownMethod.Error() {
		t.Errorf("expected test error, got %v", err)
	}
}

func TestErr(t *testing.T) {
	c, err := testClient()
	if err != nil {
		t.Fatalf("error creating test client: %v", err)
	}
	defer c.Close()

	var r testResult
	if err := c.Call("test.Err", &testArg{}, &r); err == nil {
		t.Errorf("expected non-nil err, got nil")
	} else if err.Error() != "test error" {
		t.Errorf("expected test error, got %v", err)
	}
}

func TestSendFile(t *testing.T) {
	c, err := testClient()
	if err != nil {
		t.Fatalf("error creating test client: %v", err)
	}
	defer c.Close()

	var r testResult
	if err := c.Call("test.FailNoFile", &testArg{}, &r); err == nil {
		t.Errorf("expected non-nil err, got nil")
	}
	if err := c.Call("test.FailNoFile", &testArg{FilePayload: FilePayload{Files: []*os.File{os.Stdin, os.Stdout, os.Stdin}}}, &r); err != nil {
		t.Errorf("expected nil err, got %v", err)
	}
}

func TestRecvFile(t *testing.T) {
	c, err := testClient()
	if err != nil {
		t.Fatalf("error creating test client: %v", err)
	}
	defer c.Close()

	var r testResult
	if err := c.Call("test.SendFile", &testArg{}, &r); err != nil {
		t.Errorf("expected nil err, got %v", err)
	}
	if r.Files == nil {
		t.Errorf("expected file, got nil")
	}
}

func TestShutdown(t *testing.T) {
	serverSock, clientSock, err := unet.SocketPair(false)
	if err != nil {
		t.Fatalf("error creating test client: %v", err)
	}
	clientSock.Close()

	s := NewServer()
	if err := s.Handle(serverSock); err == nil {
		t.Errorf("expected non-nil err, got nil")
	}
}

func TestTooManyFiles(t *testing.T) {
	c, err := testClient()
	if err != nil {
		t.Fatalf("error creating test client: %v", err)
	}
	defer c.Close()

	var r testResult
	var a testArg
	for i := 0; i <= maxFiles; i++ {
		a.Files = append(a.Files, os.Stdin)
	}

	// Client-side error.
	if err := c.Call("test.Func", &a, &r); err != ErrTooManyFiles {
		t.Errorf("expected ErrTooManyFiles, got %v", err)
	}

	// Server-side error.
	if err := c.Call("test.TooManyFiles", &testArg{}, &r); err == nil {
		t.Errorf("expected non-nil err, got nil")
	} else if err.Error() != "too many files" {
		t.Errorf("expected too many files, got %v", err.Error())
	}
}

func TestServeOne(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		transportErr error
		handlerErr   error
	}{
		{name: "success", method: "test.Func"},
		{name: "handler error", method: "test.Err", handlerErr: errTest},
		{name: "disconnect", transportErr: io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverSock, clientSock, err := unet.SocketPair(false)
			if err != nil {
				t.Fatal(err)
			}
			s := NewServer()
			s.Register(test{})
			type result struct{ transportErr, handlerErr error }
			done := make(chan result, 1)
			go func() {
				transportErr, handlerErr := s.ServeOne(serverSock)
				done <- result{transportErr, handlerErr}
			}()

			c := NewClient(clientSock)
			if tc.method == "" {
				c.Close()
			} else {
				defer c.Close()
				var r testResult
				err := c.Call(tc.method, &testArg{StringArg: "hello"}, &r)
				if tc.handlerErr != nil {
					if err == nil || err.Error() != tc.handlerErr.Error() {
						t.Errorf("Call = %v, want %v", err, tc.handlerErr)
					}
				} else if err != nil || r.StringResult != "hello" {
					t.Errorf("Call returned %q, %v; want hello, nil", r.StringResult, err)
				}
			}
			select {
			case got := <-done:
				if !errors.Is(got.transportErr, tc.transportErr) || !errors.Is(got.handlerErr, tc.handlerErr) {
					t.Errorf("ServeOne = (%v, %v), want (%v, %v)", got.transportErr, got.handlerErr, tc.transportErr, tc.handlerErr)
				}
				if serverSock.FD() >= 0 {
					serverSock.Close()
					t.Error("ServeOne left the socket open")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ServeOne did not return")
			}
		})
	}
}

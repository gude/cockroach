// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied.  See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Andrew Bonventre (andybons@gmail.com)

package server

import (
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/cockroachdb/cockroach/storage"
	"github.com/golang/glog"
)

var (
	s              *server
	serverTestOnce sync.Once
)

func init() {
	// We update these with the actual port once the servers
	// have been launched for the purpose of this test.
	*httpAddr = "127.0.0.1:0"
	*rpcAddr = "127.0.0.1:0"
}

func startServer() *server {
	serverTestOnce.Do(func() {
		s, err := newServer()
		if err != nil {
			glog.Fatal(err)
		}
		engines := []storage.Engine{storage.NewInMem(storage.Attributes{}, 1<<20)}
		if _, err := BootstrapCluster("cluster-1", engines[0]); err != nil {
			glog.Fatal(err)
		}
		err = s.start(engines, true) // TODO(spencer): should shutdown server.
		if err != nil {
			glog.Fatalf("Could not start server: %s", err)
		}

		// Update the configuration variables to reflect the actual
		// sockets bound during this test.
		*httpAddr = (*s.httpListener).Addr().String()
		*rpcAddr = s.rpc.Addr().String()
		glog.Infof("Test server listening on http: %s, rpc: %s", *httpAddr, *rpcAddr)
	})
	return s
}

// createTempDirs creates "count" temporary directories and returns
// the paths to each as a slice.
func createTempDirs(count int, t *testing.T) []string {
	tmp := make([]string, count)
	for i := 0; i < count; i++ {
		var err error
		if tmp[i], err = ioutil.TempDir("", "_server_test"); err != nil {
			t.Fatal(err)
		}
	}
	return tmp
}

// resetTestData recursively removes all files written to the
// directories specified as parameters.
func resetTestData(dirs []string) {
	for _, dir := range dirs {
		os.RemoveAll(dir)
	}
}

// TestInitEngine tests whether the data directory string is parsed correctly.
func TestInitEngine(t *testing.T) {
	tmp := createTempDirs(5, t)
	defer resetTestData(tmp)

	testCases := []struct {
		key       string             // data directory
		expAttrs  storage.Attributes // attributes for engine
		wantError bool               // do we expect an error from this key?
		isMem     bool               // is the engine in-memory?
	}{
		{"mem=1000", storage.Attributes([]string{"mem"}), false, true},
		{"ssd=1000", storage.Attributes([]string{"ssd"}), false, true},
		{fmt.Sprintf("ssd=%s", tmp[0]), storage.Attributes([]string{"ssd"}), false, false},
		{fmt.Sprintf("hdd=%s", tmp[1]), storage.Attributes([]string{"hdd"}), false, false},
		{fmt.Sprintf("mem=%s", tmp[2]), storage.Attributes([]string{"mem"}), false, false},
		{fmt.Sprintf("abc=%s", tmp[3]), storage.Attributes([]string{"abc"}), false, false},
		{fmt.Sprintf("hdd:7200rpm=%s", tmp[4]), storage.Attributes([]string{"hdd", "7200rpm"}), false, false},
		{"hdd=/dev/null", storage.Attributes{}, true, false},
		{"", storage.Attributes{}, true, false},
		{"  ", storage.Attributes{}, true, false},
		{"arbitrarystring", storage.Attributes{}, true, false},
		{"mem=", storage.Attributes{}, true, false},
		{"ssd=", storage.Attributes{}, true, false},
		{"hdd=", storage.Attributes{}, true, false},
	}
	for _, spec := range testCases {
		engines, err := initEngines(spec.key)
		if err == nil {
			if spec.wantError {
				t.Fatalf("invalid engine spec '%v' erroneously accepted: %+v", spec.key, spec)
			}
			if len(engines) != 1 {
				t.Fatalf("unexpected number of engines: %d: %+v", len(engines), spec)
			}
			engine := engines[0]
			if engine.Attrs().SortedString() != spec.expAttrs.SortedString() {
				t.Errorf("wrong engine attributes, expected %v but got %v: %+v", spec.expAttrs, engine.Attrs(), spec)
			}
			_, ok := engine.(*storage.InMem)
			if spec.isMem != ok {
				t.Errorf("expected in memory? %b, got %b: %+v", spec.isMem, ok, spec)
			}
		} else if !spec.wantError {
			t.Errorf("expected no error, got %v: %+v", err, spec)
		}
	}
}

// TestInitEngines tests whether multiple engines specified as a
// single comma-separated list are parsed correctly.
func TestInitEngines(t *testing.T) {
	tmp := createTempDirs(2, t)
	defer resetTestData(tmp)

	stores := fmt.Sprintf("mem=1000,mem:ddr3=1000,ssd=%s,hdd:7200rpm=%s", tmp[0], tmp[1])
	expEngines := []struct {
		attrs storage.Attributes
		isMem bool
	}{
		{storage.Attributes([]string{"mem"}), true},
		{storage.Attributes([]string{"mem", "ddr3"}), true},
		{storage.Attributes([]string{"ssd"}), false},
		{storage.Attributes([]string{"hdd", "7200rpm"}), false},
	}

	engines, err := initEngines(stores)
	if err != nil {
		t.Fatal(err)
	}
	if len(engines) != len(expEngines) {
		t.Errorf("number of engines parsed %d != expected %d", len(engines), len(expEngines))
	}
	for i, engine := range engines {
		if engine.Attrs().SortedString() != expEngines[i].attrs.SortedString() {
			t.Errorf("wrong engine attributes, expected %v but got %v: %+v", expEngines[i].attrs, engine.Attrs(), expEngines[i])
		}
		_, ok := engine.(*storage.InMem)
		if expEngines[i].isMem != ok {
			t.Errorf("expected in memory? %b, got %b: %+v", expEngines[i].isMem, ok, expEngines[i])
		}
	}
}

// TestHealthz verifies that /_admin/healthz does, in fact, return "ok"
// as expected.
func TestHealthz(t *testing.T) {
	startServer()
	url := "http://" + *httpAddr + "/_admin/healthz"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("error requesting healthz at %s: %s", url, err)
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("could not read response body: %s", err)
	}
	expected := "ok"
	if !strings.Contains(string(b), expected) {
		t.Errorf("expected body to contain %q, got %q", expected, string(b))
	}
}

// TestGzip hits the /_admin/healthz endpoint while explicitly disabling
// decompression on a custom client's Transport and setting it
// conditionally via the request's Accept-Encoding headers.
func TestGzip(t *testing.T) {
	startServer()
	client := http.Client{
		Transport: &http.Transport{
			Proxy:              http.ProxyFromEnvironment,
			DisableCompression: true,
		},
	}
	req, err := http.NewRequest("GET", "http://"+*httpAddr+"/_admin/healthz", nil)
	if err != nil {
		t.Fatalf("could not create request: %s", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("could not make request to %s: %s", req.URL, err)
	}
	defer resp.Body.Close()
	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("could not read response body: %s", err)
	}
	expected := "ok"
	if !strings.Contains(string(b), expected) {
		t.Errorf("expected body to contain %q, got %q", expected, string(b))
	}
	// Test for gzip explicitly.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("could not make request to %s: %s", req.URL, err)
	}
	defer resp.Body.Close()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("could not create new gzip reader: %s", err)
	}
	b, err = ioutil.ReadAll(gz)
	if err != nil {
		t.Fatalf("could not read gzipped response body: %s", err)
	}
	if !strings.Contains(string(b), expected) {
		t.Errorf("expected body to contain %q, got %q", expected, string(b))
	}
}

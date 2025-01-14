// Copyright 2019 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// Used to setup superclusters for tests.
type supercluster struct {
	clusters []*cluster
}

func (sc *supercluster) shutdown() {
	for _, c := range sc.clusters {
		shutdownCluster(c)
	}
}

const digits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
const base = 36
const cnlen = 8

func randClusterName() string {
	var name []byte
	rn := rand.Int63()
	for i := 0; i < cnlen; i++ {
		name = append(name, digits[rn%base])
		rn /= base
	}
	return string(name[:cnlen])
}

func createSuperCluster(t *testing.T, numServersPer, numClusters int) *supercluster {
	clusters := []*cluster{}

	for i := 0; i < numClusters; i++ {
		// Pick cluster name and setup default accounts.
		c := createClusterEx(t, true, randClusterName(), numServersPer, clusters...)
		clusters = append(clusters, c)
	}
	return &supercluster{clusters}
}

func (sc *supercluster) setupLatencyTracking(t *testing.T, p int) {
	t.Helper()
	for _, c := range sc.clusters {
		for _, s := range c.servers {
			foo, err := s.LookupAccount("FOO")
			if err != nil {
				t.Fatalf("Error looking up account 'FOO': %v", err)
			}
			bar, err := s.LookupAccount("BAR")
			if err != nil {
				t.Fatalf("Error looking up account 'BAR': %v", err)
			}
			if err := foo.AddServiceExport("ngs.usage.*", nil); err != nil {
				t.Fatalf("Error adding service export to 'FOO': %v", err)
			}
			if err := foo.TrackServiceExportWithSampling("ngs.usage.*", "results", p); err != nil {
				t.Fatalf("Error adding latency tracking to 'FOO': %v", err)
			}
			if err := bar.AddServiceImport(foo, "ngs.usage", "ngs.usage.bar"); err != nil {
				t.Fatalf("Error adding latency tracking to 'FOO': %v", err)
			}
		}
	}
}

func clientConnectWithName(t *testing.T, opts *server.Options, user, appname string) *nats.Conn {
	t.Helper()
	url := fmt.Sprintf("nats://%s:pass@%s:%d", user, opts.Host, opts.Port)
	nc, err := nats.Connect(url, nats.Name(appname))
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	return nc
}

func clientConnect(t *testing.T, opts *server.Options, user string) *nats.Conn {
	t.Helper()
	return clientConnectWithName(t, opts, user, "")
}

func checkServiceLatency(t *testing.T, sl server.ServiceLatency, start time.Time, serviceTime time.Duration) {
	t.Helper()

	startDelta := sl.RequestStart.Sub(start)
	if startDelta > 5*time.Millisecond {
		t.Fatalf("Bad start delta %v", startDelta)
	}
	if sl.ServiceLatency < serviceTime {
		t.Fatalf("Bad service latency: %v", sl.ServiceLatency)
	}
	if sl.TotalLatency < sl.ServiceLatency {
		t.Fatalf("Bad total latency: %v", sl.ServiceLatency)
	}
	// We should have NATS latency here that is non-zero with real clients.
	if sl.NATSLatency == 0 {
		t.Fatalf("Expected non-zero NATS latency")
	}
	// Make sure they add up
	if sl.TotalLatency != sl.ServiceLatency+sl.NATSLatency {
		t.Fatalf("Numbers do not add up: %+v", sl)
	}

}

func TestServiceLatencySingleServerConnect(t *testing.T) {
	sc := createSuperCluster(t, 3, 2)
	defer sc.shutdown()

	// Now add in new service export to FOO and have bar import that with tracking enabled.
	sc.setupLatencyTracking(t, 100)

	// Now we can setup and test, do single node only first.
	// This is the service provider.
	nc := clientConnect(t, sc.clusters[0].opts[0], "foo")
	defer nc.Close()

	// The service listener.
	serviceTime := 25 * time.Millisecond
	nc.Subscribe("ngs.usage.*", func(msg *nats.Msg) {
		time.Sleep(serviceTime)
		msg.Respond([]byte("22 msgs"))
	})

	// Listen for metrics
	rsub, _ := nc.SubscribeSync("results")

	// Requestor
	nc2 := clientConnect(t, sc.clusters[0].opts[0], "bar")
	defer nc.Close()

	// Send the request.
	start := time.Now()
	_, err := nc2.Request("ngs.usage", []byte("1h"), time.Second)
	if err != nil {
		t.Fatalf("Expected a response")
	}

	var sl server.ServiceLatency
	rmsg, _ := rsub.NextMsg(time.Second)
	json.Unmarshal(rmsg.Data, &sl)

	checkServiceLatency(t, sl, start, serviceTime)
}

func connRTT(nc *nats.Conn) time.Duration {
	// Do 5x to flatten
	total := time.Duration(0)
	for i := 0; i < 5; i++ {
		start := time.Now()
		nc.Flush()
		total += time.Since(start)
	}
	return total / 5
}

func TestServiceLatencyRemoteConnect(t *testing.T) {
	sc := createSuperCluster(t, 3, 2)
	defer sc.shutdown()

	// Now add in new service export to FOO and have bar import that with tracking enabled.
	sc.setupLatencyTracking(t, 100)

	// Now we can setup and test, do single node only first.
	// This is the service provider.
	nc := clientConnect(t, sc.clusters[0].opts[0], "foo")
	defer nc.Close()

	// The service listener.
	serviceTime := 25 * time.Millisecond
	nc.Subscribe("ngs.usage.*", func(msg *nats.Msg) {
		time.Sleep(serviceTime)
		msg.Respond([]byte("22 msgs"))
	})

	// Listen for metrics
	rsub, _ := nc.SubscribeSync("results")

	// Same Cluster Requestor
	nc2 := clientConnect(t, sc.clusters[0].opts[2], "bar")
	defer nc.Close()

	// Send the request.
	start := time.Now()
	_, err := nc2.Request("ngs.usage", []byte("1h"), time.Second)
	if err != nil {
		t.Fatalf("Expected a response")
	}

	var sl server.ServiceLatency
	rmsg, err := rsub.NextMsg(time.Second)
	if err != nil {
		t.Fatalf("Error getting latency measurement: %v", err)
	}
	json.Unmarshal(rmsg.Data, &sl)
	checkServiceLatency(t, sl, start, serviceTime)

	// Lastly here, we need to make sure we are properly tracking the extra hops.
	// We will make sure that NATS latency is close to what we see from the outside in terms of RTT.
	if crtt := connRTT(nc) + connRTT(nc2); sl.NATSLatency < crtt {
		t.Fatalf("Not tracking second measurement for NATS latency across servers: %v vs %v", sl.NATSLatency, crtt)
	}

	// Gateway Requestor
	nc2 = clientConnect(t, sc.clusters[1].opts[1], "bar")
	defer nc.Close()

	// Send the request.
	start = time.Now()
	_, err = nc2.Request("ngs.usage", []byte("1h"), time.Second)
	if err != nil {
		t.Fatalf("Expected a response")
	}

	rmsg, err = rsub.NextMsg(time.Second)
	if err != nil {
		t.Fatalf("Error getting latency measurement: %v", err)
	}
	json.Unmarshal(rmsg.Data, &sl)
	checkServiceLatency(t, sl, start, serviceTime)

	// Lastly here, we need to make sure we are properly tracking the extra hops.
	// We will make sure that NATS latency is close to what we see from the outside in terms of RTT.
	if crtt := connRTT(nc) + connRTT(nc2); sl.NATSLatency < crtt {
		t.Fatalf("Not tracking second measurement for NATS latency across servers: %v vs %v", sl.NATSLatency, crtt)
	}
}

func TestServiceLatencySampling(t *testing.T) {
	sc := createSuperCluster(t, 3, 2)
	defer sc.shutdown()

	// Now add in new service export to FOO and have bar import that with tracking enabled.
	sc.setupLatencyTracking(t, 50)

	// Now we can setup and test, do single node only first.
	// This is the service provider.
	nc := clientConnect(t, sc.clusters[0].opts[0], "foo")
	defer nc.Close()

	// The service listener.
	nc.Subscribe("ngs.usage.*", func(msg *nats.Msg) {
		msg.Respond([]byte("22 msgs"))
	})

	// Listen for metrics
	received := int32(0)

	nc.Subscribe("results", func(msg *nats.Msg) {
		atomic.AddInt32(&received, 1)
	})

	// Same Cluster Requestor
	nc2 := clientConnect(t, sc.clusters[0].opts[2], "bar")
	defer nc.Close()

	toSend := 1000
	for i := 0; i < toSend; i++ {
		nc2.Request("ngs.usage", []byte("1h"), time.Second)
	}
	// Wait for results to flow in.
	time.Sleep(100 * time.Millisecond)

	mid := toSend / 2
	delta := toSend / 10 // 10%
	got := int(atomic.LoadInt32(&received))

	if got > mid+delta || got < mid-delta {
		t.Fatalf("Sampling number incorrect: %d vs %d", mid, got)
	}
}

func TestServiceLatencyWithName(t *testing.T) {
	sc := createSuperCluster(t, 1, 1)
	defer sc.shutdown()

	// Now add in new service export to FOO and have bar import that with tracking enabled.
	sc.setupLatencyTracking(t, 100)

	opts := sc.clusters[0].opts[0]

	nc := clientConnectWithName(t, opts, "foo", "dlc22")
	defer nc.Close()

	// The service listener.
	nc.Subscribe("ngs.usage.*", func(msg *nats.Msg) {
		msg.Respond([]byte("22 msgs"))
	})

	// Listen for metrics
	rsub, _ := nc.SubscribeSync("results")

	nc2 := clientConnect(t, opts, "bar")
	defer nc.Close()
	nc2.Request("ngs.usage", []byte("1h"), time.Second)

	var sl server.ServiceLatency
	rmsg, _ := rsub.NextMsg(time.Second)
	json.Unmarshal(rmsg.Data, &sl)

	// Make sure we have AppName set.
	if sl.AppName != "dlc22" {
		t.Fatalf("Expected to have AppName set correctly, %q vs %q", "dlc22", sl.AppName)
	}
}

func TestServiceLatencyWithNameMultiServer(t *testing.T) {
	sc := createSuperCluster(t, 3, 2)
	defer sc.shutdown()

	// Now add in new service export to FOO and have bar import that with tracking enabled.
	sc.setupLatencyTracking(t, 100)

	nc := clientConnectWithName(t, sc.clusters[0].opts[1], "foo", "dlc22")
	defer nc.Close()

	// The service listener.
	nc.Subscribe("ngs.usage.*", func(msg *nats.Msg) {
		msg.Respond([]byte("22 msgs"))
	})

	// Listen for metrics
	rsub, _ := nc.SubscribeSync("results")

	nc2 := clientConnect(t, sc.clusters[1].opts[1], "bar")
	defer nc.Close()
	nc2.Request("ngs.usage", []byte("1h"), time.Second)

	var sl server.ServiceLatency
	rmsg, _ := rsub.NextMsg(time.Second)
	json.Unmarshal(rmsg.Data, &sl)

	// Make sure we have AppName set.
	if sl.AppName != "dlc22" {
		t.Fatalf("Expected to have AppName set correctly, %q vs %q", "dlc22", sl.AppName)
	}
}

func TestServiceLatencyWithQueueSubscribersAndNames(t *testing.T) {
	numServers := 3
	numClusters := 3
	sc := createSuperCluster(t, numServers, numClusters)
	defer sc.shutdown()

	// Now add in new service export to FOO and have bar import that with tracking enabled.
	sc.setupLatencyTracking(t, 100)

	selectServer := func() *server.Options {
		si, ci := rand.Int63n(int64(numServers)), rand.Int63n(int64(numServers))
		return sc.clusters[ci].opts[si]
	}

	sname := func(i int) string {
		return fmt.Sprintf("SERVICE-%d", i+1)
	}

	numResponders := 5

	// Create 10 queue subscribers for the service. Randomly select the server.
	for i := 0; i < numResponders; i++ {
		nc := clientConnectWithName(t, selectServer(), "foo", sname(i))
		nc.QueueSubscribe("ngs.usage.*", "SERVICE", func(msg *nats.Msg) {
			time.Sleep(time.Duration(rand.Int63n(10)) * time.Millisecond)
			msg.Respond([]byte("22 msgs"))
		})
		nc.Flush()
	}

	doRequest := func() {
		nc := clientConnect(t, selectServer(), "bar")
		if _, err := nc.Request("ngs.usage", []byte("1h"), time.Second); err != nil {
			t.Fatalf("Failed to get request response: %v", err)
		}
		nc.Close()
	}

	// To collect the metrics
	nc := clientConnect(t, sc.clusters[0].opts[0], "foo")
	defer nc.Close()

	results := make(map[string]time.Duration)
	var rlock sync.Mutex
	ch := make(chan (bool))
	received := int32(0)
	toSend := int32(100)

	// Capture the results.
	nc.Subscribe("results", func(msg *nats.Msg) {
		var sl server.ServiceLatency
		json.Unmarshal(msg.Data, &sl)
		rlock.Lock()
		results[sl.AppName] += sl.ServiceLatency
		rlock.Unlock()
		if r := atomic.AddInt32(&received, 1); r >= toSend {
			ch <- true
		}
	})
	nc.Flush()

	// Send 100 requests from random locations.
	for i := 0; i < 100; i++ {
		doRequest()
	}

	// Wait on all results.
	<-ch

	rlock.Lock()
	defer rlock.Unlock()

	// Make sure each total is generally over 10ms
	thresh := 10 * time.Millisecond
	for i := 0; i < numResponders; i++ {
		if rl := results[sname(i)]; rl < thresh {
			t.Fatalf("Total for %q is less then threshold: %v vs %v", sname(i), thresh, rl)
		}
	}
}

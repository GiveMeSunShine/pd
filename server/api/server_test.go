// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	. "github.com/pingcap/check"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/pd/server"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

var (
	clusterID = uint64(time.Now().Unix())
	store     = &metapb.Store{
		Id:      1,
		Address: "localhost",
	}
	peers = []*metapb.Peer{
		{
			Id:      2,
			StoreId: store.GetId(),
		},
	}
	region = &metapb.Region{
		Id: 8,
		RegionEpoch: &metapb.RegionEpoch{
			ConfVer: 1,
			Version: 1,
		},
		Peers: peers,
	}
	unixClient = newUnixSocketClient()
)

func TestAPIServer(t *testing.T) {
	TestingT(t)
}

func newUnixSocketClient() *http.Client {
	tr := &http.Transport{
		Dial: unixDial,
	}
	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: tr,
	}

	return client
}

func mustUnixAddrToHTTPAddr(c *C, addr string) string {
	u, err := url.Parse(addr)
	c.Assert(err, IsNil)
	u.Scheme = "http"
	return u.String()
}

var stripUnix = strings.NewReplacer("unix://", "")

func cleanServer(cfg *server.Config) {
	// Clean data directory
	os.RemoveAll(cfg.DataDir)

	// Clean unix sockets
	os.Remove(stripUnix.Replace(cfg.PeerUrls))
	os.Remove(stripUnix.Replace(cfg.ClientUrls))
	os.Remove(stripUnix.Replace(cfg.AdvertisePeerUrls))
	os.Remove(stripUnix.Replace(cfg.AdvertiseClientUrls))
}

type cleanUpFunc func()

func mustNewServer(c *C) (*server.Server, cleanUpFunc) {
	_, svrs, cleanup := mustNewCluster(c, 1)
	return svrs[0], cleanup
}

func mustNewCluster(c *C, num int) ([]*server.Config, []*server.Server, cleanUpFunc) {
	svrs := make([]*server.Server, 0, num)
	cfgs := server.NewTestMultiConfig(num)

	ch := make(chan *server.Server, num)
	for _, cfg := range cfgs {
		go func(cfg *server.Config) {
			s := server.CreateServer(cfg)
			e := s.StartEtcd(NewHandler(s))
			c.Assert(e, IsNil)
			go s.Run()
			ch <- s
		}(cfg)
	}

	for i := 0; i < num; i++ {
		svr := <-ch
		svrs = append(svrs, svr)
	}
	close(ch)

	// wait etcds and http servers
	mustWaitLeader(c, svrs)

	// clean up
	clean := func() {
		for _, s := range svrs {
			s.Close()
		}
		for _, cfg := range cfgs {
			cleanServer(cfg)
		}
	}

	return cfgs, svrs, clean
}

func mustWaitLeader(c *C, svrs []*server.Server) *server.Server {
	for i := 0; i < 100; i++ {
		for _, svr := range svrs {
			if svr.IsLeader() {
				return svr
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	c.Fatal("no leader")
	return nil
}

func newRequestHeader(clusterID uint64) *pdpb.RequestHeader {
	return &pdpb.RequestHeader{
		ClusterId: clusterID,
	}
}

var unixStripper = strings.NewReplacer("unix://", "", "unixs://", "")

func unixGrpcDialer(addr string, timeout time.Duration) (net.Conn, error) {
	sock, err := net.DialTimeout("unix", unixStripper.Replace(addr), timeout)
	return sock, err
}

func mustNewGrpcClient(c *C, addr string) pdpb.PDClient {
	conn, err := grpc.Dial(addr, grpc.WithInsecure(),
		grpc.WithDialer(unixGrpcDialer))

	c.Assert(err, IsNil)
	return pdpb.NewPDClient(conn)
}
func mustBootstrapCluster(c *C, s *server.Server) {
	grpcPDClient := mustNewGrpcClient(c, s.GetAddr())
	req := &pdpb.BootstrapRequest{
		Header: newRequestHeader(s.ClusterID()),
		Store:  store,
		Region: region,
	}
	resp, err := grpcPDClient.Bootstrap(context.Background(), req)
	c.Assert(err, IsNil)
	c.Assert(resp.GetHeader().GetError().GetType(), Equals, pdpb.ErrorType_OK)
}

func mustPutStore(c *C, s *server.Server, store *metapb.Store) {
	grpcPDClient := mustNewGrpcClient(c, s.GetAddr())
	req := &pdpb.PutStoreRequest{
		Header: newRequestHeader(s.ClusterID()),
		Store:  store,
	}
	resp, err := grpcPDClient.PutStore(context.Background(), req)
	c.Assert(err, IsNil)
	c.Assert(resp.GetHeader().GetError().GetType(), Equals, pdpb.ErrorType_OK)
}

func mustRegionHeartBeat(c *C, client pdpb.PD_RegionHeartbeatClient, clusterID uint64, region *server.RegionInfo) {
	req := &pdpb.RegionHeartbeatRequest{
		Header: newRequestHeader(clusterID),
		Region: region.Region,
		Leader: region.Leader,
	}

	err := client.Send(req)
	c.Assert(err, IsNil)

	// Sleep a while to make sure the message is processed by server.
	time.Sleep(time.Millisecond * 200)
}

func readJSONWithURL(url string, data interface{}) error {
	resp, err := unixClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return readJSON(resp.Body, data)
}

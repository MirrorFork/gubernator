/*
Copyright 2018-2020 Technologies Inc

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

package gubernator

import (
	"context"
	"net"
	"net/http"
	"strings"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/mailgun/holster/v3/etcdutil"
	"github.com/mailgun/holster/v3/syncutil"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
)

var DebugEnabled = false

type DaemonConfig struct {
	GRPCListenAddress string
	HTTPListenAddress string
	AdvertiseAddress  string
	EtcdKeyPrefix     string
	CacheSize         int

	// Deprecated, use AdvertiseAddress instead
	EtcdAdvertiseAddress string

	// Etcd configuration used to find peers
	EtcdConf etcd.Config

	// Configure how behaviours behave
	Behaviors BehaviorConfig

	// K8s configuration used to find peers inside a K8s cluster
	K8PoolConf K8sPoolConfig
}

type Daemon struct {
	GRPCListener net.Listener
	HTTPListener net.Listener
	V1Server     *Instance

	pool         PoolInterface
	conf         DaemonConfig
	httpSrv      *http.Server
	grpcSrv      *grpc.Server
	wg           syncutil.WaitGroup
	statsHandler *GRPCStatsHandler
	promRegister *prometheus.Registry
}

// NewDaemon starts a new gubernator daemon according to the provided DaemonConfig.
// This function will block until the daemon responds to connections as specified
// by GRPCListenAddress and HTTPListenAddress
func NewDaemon(ctx context.Context, conf DaemonConfig) (*Daemon, error) {
	s := Daemon{conf: conf}
	var err error

	// Handle deprecated advertise address
	s.conf.AdvertiseAddress = s.conf.EtcdAdvertiseAddress

	// The LRU cache we store rate limits in
	cache := NewLRUCache(conf.CacheSize)

	// cache also implements prometheus.Collector interface
	s.promRegister = prometheus.NewRegistry()
	s.promRegister.Register(cache)

	// Handler to collect duration and API access metrics for GRPC
	s.statsHandler = NewGRPCStatsHandler()
	s.promRegister.Register(s.statsHandler)

	// New GRPC server
	s.grpcSrv = grpc.NewServer(
		grpc.StatsHandler(s.statsHandler),
		grpc.MaxRecvMsgSize(1024*1024))

	// Registers a new gubernator instance with the GRPC server
	s.V1Server, err = New(Config{
		GRPCServer: s.grpcSrv,
		Cache:      cache,
	})
	if err != nil {
		return nil, errors.Wrap(err, "while creating new gubernator instance")
	}

	// v1Server instance also implements prometheus.Collector interface
	s.promRegister.Register(s.V1Server)

	s.GRPCListener, err = net.Listen("tcp", conf.GRPCListenAddress)
	if err != nil {
		return nil, errors.Wrap(err, "while starting GRPC listener")
	}

	// Start serving GRPC Requests
	s.wg.Go(func() {
		log.Infof("Gubernator Listening on %s ...", conf.GRPCListenAddress)
		if err := s.grpcSrv.Serve(s.GRPCListener); err != nil {
			log.WithError(err).Error("while starting GRPC server")
		}
	})

	if conf.K8PoolConf.Enabled {
		// Source our list of peers from kubernetes endpoint API
		conf.K8PoolConf.OnUpdate = s.V1Server.SetPeers
		s.pool, err = NewK8sPool(conf.K8PoolConf)
		if err != nil {
			return nil, errors.Wrap(err, "while querying kubernetes API")
		}
	}

	if conf.EtcdConf.Endpoints != nil {
		// Register ourselves with other peers via ETCD
		etcdClient, err := etcdutil.NewClient(&conf.EtcdConf)
		if err != nil {
			return nil, errors.Wrap(err, "while connecting to etcd")
		}

		s.pool, err = NewEtcdPool(EtcdPoolConfig{
			AdvertiseAddress: conf.AdvertiseAddress,
			OnUpdate:         s.V1Server.SetPeers,
			Client:           etcdClient,
			BaseKey:          conf.EtcdKeyPrefix,
		})
		if err != nil {
			return nil, errors.Wrap(err, "while registering with ETCD pool")
		}
	}

	// Setup an JSON Gateway API for our GRPC methods
	gateway := runtime.NewServeMux()
	err = RegisterV1HandlerFromEndpoint(ctx, gateway,
		conf.GRPCListenAddress, []grpc.DialOption{grpc.WithInsecure()})
	if err != nil {
		return nil, errors.Wrap(err, "while registering GRPC gateway handler")
	}

	// Serve the JSON Gateway and metrics handlers via standard HTTP/1
	mux := http.NewServeMux()

	mux.Handle("/metrics", promhttp.InstrumentMetricHandler(
		s.promRegister, promhttp.HandlerFor(s.promRegister, promhttp.HandlerOpts{}),
	))
	mux.Handle("/", gateway)
	s.httpSrv = &http.Server{Addr: conf.HTTPListenAddress, Handler: mux}

	s.HTTPListener, err = net.Listen("tcp", conf.HTTPListenAddress)
	if err != nil {
		return nil, errors.Wrap(err, "while starting HTTP listener")
	}

	s.wg.Go(func() {
		log.Infof("HTTP Gateway Listening on %s ...", conf.HTTPListenAddress)
		if err := s.httpSrv.Serve(s.HTTPListener); err != nil {
			log.WithError(err).Error("while starting HTTP server")
		}
	})

	// Validate we can reach the GRPC and HTTP endpoints before returning
	if err := WaitForConnect(ctx, []string{conf.HTTPListenAddress, conf.GRPCListenAddress}); err != nil {
		return nil, err
	}

	return &s, nil
}

// Close gracefully closes all server connections and listening sockets
func (s *Daemon) Close() {
	s.pool.Close()
	s.httpSrv.Shutdown(context.Background())
	s.grpcSrv.GracefulStop()
	s.wg.Stop()
	s.statsHandler.Close()
}

// SetPeers sets the peers for this daemon
func (s *Daemon) SetPeers(peers []PeerInfo) {
	s.V1Server.SetPeers(peers)
}

// Peers returns the peers this daemon knows about
func (s *Daemon) Peers() []PeerInfo {
	var peers []PeerInfo
	for _, client := range s.V1Server.GetPeerList() {
		peers = append(peers, client.PeerInfo())
	}
	return peers
}

// WaitForConnect returns nil if the list of addresses is listening for connections; will block until context is cancelled.
func WaitForConnect(ctx context.Context, addresses []string) error {
	var d net.Dialer
	var errs []error
	for {
		errs = nil
		for _, addr := range addresses {
			if addr == "" {
				continue
			}

			conn, err := d.DialContext(ctx, "tcp", addr)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			conn.Close()
		}

		if len(errs) == 0 {
			break
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if len(errs) != 0 {
		var errStrings []string
		for _, err := range errs {
			errStrings = append(errStrings, err.Error())
		}
		return errors.New(strings.Join(errStrings, "\n"))
	}
	return nil
}

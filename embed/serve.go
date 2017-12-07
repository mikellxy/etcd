// Copyright 2015 The etcd Authors
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

package embed

import (
	"context"
	"io/ioutil"
	defaultLog "log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/etcd/etcdserver"
	"github.com/coreos/etcd/etcdserver/api/v3client"
	"github.com/coreos/etcd/etcdserver/api/v3election"
	"github.com/coreos/etcd/etcdserver/api/v3election/v3electionpb"
	v3electiongw "github.com/coreos/etcd/etcdserver/api/v3election/v3electionpb/gw"
	"github.com/coreos/etcd/etcdserver/api/v3lock"
	"github.com/coreos/etcd/etcdserver/api/v3lock/v3lockpb"
	v3lockgw "github.com/coreos/etcd/etcdserver/api/v3lock/v3lockpb/gw"
	"github.com/coreos/etcd/etcdserver/api/v3rpc"
	etcdservergw "github.com/coreos/etcd/etcdserver/etcdserverpb/gw"
	"github.com/coreos/etcd/pkg/debugutil"
	"github.com/coreos/etcd/pkg/transport"

	gw "github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/soheilhy/cmux"
	"github.com/tmc/grpc-websocket-proxy/wsproxy"
	"golang.org/x/net/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type serveCtx struct {
	l        net.Listener
	addr     string
	secure   bool
	insecure bool

	ctx    context.Context
	cancel context.CancelFunc

	userHandlers    map[string]http.Handler
	serviceRegister func(*grpc.Server)

	secureHTTPServer    *http.Server
	secureGrpcServerC   chan *grpc.Server
	insecureGrpcServerC chan *grpc.Server
}

func newServeCtx() *serveCtx {
	ctx, cancel := context.WithCancel(context.Background())
	return &serveCtx{
		ctx:                 ctx,
		cancel:              cancel,
		userHandlers:        make(map[string]http.Handler),
		secureGrpcServerC:   make(chan *grpc.Server, 1),
		insecureGrpcServerC: make(chan *grpc.Server, 1),
	}
}

// serve accepts incoming connections on the listener l,
// creating a new service goroutine for each. The service goroutines
// read requests and then call handler to reply to them.
func (sctx *serveCtx) serve(
	s *etcdserver.EtcdServer,
	tlsinfo *transport.TLSInfo,
	handler http.Handler,
	errHandler func(error),
	gopts ...grpc.ServerOption) error {
	logger := defaultLog.New(ioutil.Discard, "etcdhttp", 0)
	<-s.ReadyNotify()
	plog.Info("ready to serve client requests")

	m := cmux.New(sctx.l)
	v3c := v3client.New(s)
	servElection := v3election.NewElectionServer(v3c)
	servLock := v3lock.NewLockServer(v3c)

	if sctx.insecure {
		gs := v3rpc.Server(s, nil, gopts...)
		sctx.insecureGrpcServerC <- gs
		v3electionpb.RegisterElectionServer(gs, servElection)
		v3lockpb.RegisterLockServer(gs, servLock)
		if sctx.serviceRegister != nil {
			sctx.serviceRegister(gs)
		}
		grpcl := m.Match(cmux.HTTP2())
		go func() { errHandler(gs.Serve(grpcl)) }()

		opts := []grpc.DialOption{
			grpc.WithInsecure(),
		}
		gwmux, err := sctx.registerGateway(opts)
		if err != nil {
			return err
		}

		httpmux := sctx.createMux(gwmux, handler)

		srvhttp := &http.Server{
			Handler:  wrapMux(httpmux),
			ErrorLog: logger, // do not log user error
		}
		httpl := m.Match(cmux.HTTP1())
		go func() { errHandler(srvhttp.Serve(httpl)) }()
		plog.Noticef("serving insecure client requests on %s, this is strongly discouraged!", sctx.l.Addr().String())
	}

	if sctx.secure {
		tlscfg, tlsErr := tlsinfo.ServerConfig()
		if tlsErr != nil {
			return tlsErr
		}
		gs := v3rpc.Server(s, tlscfg, gopts...)
		sctx.secureGrpcServerC <- gs
		v3electionpb.RegisterElectionServer(gs, servElection)
		v3lockpb.RegisterLockServer(gs, servLock)
		if sctx.serviceRegister != nil {
			sctx.serviceRegister(gs)
		}
		handler = grpcHandlerFunc(gs, handler)

		dtls := tlscfg.Clone()
		// trust local server
		dtls.InsecureSkipVerify = true
		creds := credentials.NewTLS(dtls)
		opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
		gwmux, err := sctx.registerGateway(opts)
		if err != nil {
			return err
		}

		tlsl, lerr := transport.NewTLSListener(m.Match(cmux.Any()), tlsinfo)
		if lerr != nil {
			return lerr
		}
		// TODO: add debug flag; enable logging when debug flag is set
		httpmux := sctx.createMux(gwmux, handler)

		srv := &http.Server{
			Handler:   wrapMux(httpmux),
			TLSConfig: tlscfg,
			ErrorLog:  logger, // do not log user error
		}
		go func() { errHandler(srv.Serve(tlsl)) }()
		sctx.secureHTTPServer = srv

		plog.Infof("serving client requests on %s", sctx.l.Addr().String())
	}

	close(sctx.secureGrpcServerC)
	close(sctx.insecureGrpcServerC)
	return m.Serve()
}

// grpcHandlerFunc returns an http.Handler that delegates to grpcServer on incoming gRPC
// connections or otherHandler otherwise. Given in gRPC docs.
func grpcHandlerFunc(grpcServer *grpc.Server, otherHandler http.Handler) http.Handler {
	if otherHandler == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			grpcServer.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			otherHandler.ServeHTTP(w, r)
		}
	})
}

type registerHandlerFunc func(context.Context, *gw.ServeMux, *grpc.ClientConn) error

func (sctx *serveCtx) registerGateway(opts []grpc.DialOption) (*gw.ServeMux, error) {
	ctx := sctx.ctx
	conn, err := grpc.DialContext(ctx, sctx.addr, opts...)
	if err != nil {
		return nil, err
	}
	gwmux := gw.NewServeMux()

	handlers := []registerHandlerFunc{
		etcdservergw.RegisterKVHandler,
		etcdservergw.RegisterWatchHandler,
		etcdservergw.RegisterLeaseHandler,
		etcdservergw.RegisterClusterHandler,
		etcdservergw.RegisterMaintenanceHandler,
		etcdservergw.RegisterAuthHandler,
		v3lockgw.RegisterLockHandler,
		v3electiongw.RegisterElectionHandler,
	}
	for _, h := range handlers {
		if err := h(ctx, gwmux, conn); err != nil {
			return nil, err
		}
	}
	go func() {
		<-ctx.Done()
		if cerr := conn.Close(); cerr != nil {
			plog.Warningf("failed to close conn to %s: %v", sctx.l.Addr().String(), cerr)
		}
	}()

	return gwmux, nil
}

func (sctx *serveCtx) createMux(gwmux *gw.ServeMux, handler http.Handler) *http.ServeMux {
	httpmux := http.NewServeMux()
	for path, h := range sctx.userHandlers {
		httpmux.Handle(path, h)
	}

	httpmux.Handle(
		"/v3beta/",
		wsproxy.WebsocketProxy(
			gwmux,
			wsproxy.WithRequestMutator(
				// Default to the POST method for streams
				func(incoming *http.Request, outgoing *http.Request) *http.Request {
					outgoing.Method = "POST"
					return outgoing
				},
			),
		),
	)
	if handler != nil {
		httpmux.Handle("/", handler)
	}
	return httpmux
}

// wraps HTTP multiplexer to mute requests to /v3alpha
// TODO: deprecate this in 3.4 release
func wrapMux(mux *http.ServeMux) http.Handler { return &v3alphaMutator{mux: mux} }

type v3alphaMutator struct {
	mux *http.ServeMux
}

func (m *v3alphaMutator) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req != nil && req.URL != nil && strings.HasPrefix(req.URL.Path, "/v3alpha/") {
		req.URL.Path = strings.Replace(req.URL.Path, "/v3alpha/", "/v3beta/", 1)
	}
	m.mux.ServeHTTP(rw, req)
}

func (sctx *serveCtx) registerUserHandler(s string, h http.Handler) {
	if sctx.userHandlers[s] != nil {
		plog.Warningf("path %s already registered by user handler", s)
		return
	}
	sctx.userHandlers[s] = h
}

func (sctx *serveCtx) registerPprof() {
	for p, h := range debugutil.PProfHandlers() {
		sctx.registerUserHandler(p, h)
	}
}

func (sctx *serveCtx) registerTrace() {
	reqf := func(w http.ResponseWriter, r *http.Request) { trace.Render(w, r, true) }
	sctx.registerUserHandler("/debug/requests", http.HandlerFunc(reqf))
	evf := func(w http.ResponseWriter, r *http.Request) { trace.RenderEvents(w, r, true) }
	sctx.registerUserHandler("/debug/events", http.HandlerFunc(evf))
}

// Attempt to gracefully tear down gRPC server(s) and any associated mechanisms
func teardownServeCtx(sctx *serveCtx, timeout time.Duration) {
	if sctx.secure && len(sctx.secureGrpcServerC) > 0 {
		gs := <-sctx.secureGrpcServerC
		stopSecureServer(gs, sctx.secureHTTPServer, timeout)
	}

	if sctx.insecure && len(sctx.insecureGrpcServerC) > 0 {
		gs := <-sctx.insecureGrpcServerC
		stopInsecureServer(gs, timeout)
	}

	// Close any open gRPC connections
	sctx.cancel()
}

// When using grpc's ServerHandlerTransport we are responsible for gracefully
// stopping connections and shutting down.
// https://github.com/grpc/grpc-go/issues/1384#issuecomment-317124531
func stopSecureServer(gs *grpc.Server, httpSrv *http.Server, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Stop accepting new connections await pending handlers
	httpSrv.Shutdown(ctx)

	// Teardown gRPC server
	gs.Stop()
}

// Gracefully shutdown gRPC server when using HTTP2 transport.
func stopInsecureServer(gs *grpc.Server, timeout time.Duration) {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		// close listeners to stop accepting new connections,
		// will block on any existing transports
		gs.GracefulStop()
	}()
	// wait until all pending RPCs are finished
	select {
	case <-ch:
	case <-time.After(timeout):
		// took too long, manually close open transports
		// e.g. watch streams
		gs.Stop()
		// concurrent GracefulStop should be interrupted
		<-ch
	}
}

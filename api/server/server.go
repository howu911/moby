package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/errors"
	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/api/server/middleware"
	"github.com/docker/docker/api/server/router"
	"github.com/gorilla/mux"
	"golang.org/x/net/context"
)

// versionMatcher defines a variable matcher to be parsed by the router
// when a request is about to be served.
const versionMatcher = "/v{version:[0-9.]+}"

// Config provides the configuration for the API server
type Config struct {
	Logging     bool
	EnableCors  bool
	CorsHeaders string
	Version     string
	SocketGroup string
	TLSConfig   *tls.Config
}

// Server contains instance details for the server
type Server struct {
	cfg           *Config                 //apiserver的配置信息
	servers       []*HTTPServer           //httpServer结构体对象，包括http.Server和net.Listener监听器。
	routers       []router.Router         //路由表对象Route,包括Handler,Method, Path
	routerSwapper *routerSwapper          //路由交换器对象，使用新的路由交换旧的路由器
	middlewares   []middleware.Middleware //中间件
}

// New returns a new instance of the server based on the specified configuration.
// It allocates resources which will be needed for ServeAPI(ports, unix-sockets).
func New(cfg *Config) *Server {
	return &Server{
		cfg: cfg,
	}
}

// UseMiddleware appends a new middleware to the request chain.
// This needs to be called before the API routes are configured.
func (s *Server) UseMiddleware(m middleware.Middleware) {
	s.middlewares = append(s.middlewares, m)
}

// Accept sets a listener the server accepts connections into.
func (s *Server) Accept(addr string, listeners ...net.Listener) {
	for _, listener := range listeners {
		httpServer := &HTTPServer{
			srv: &http.Server{
				Addr: addr,
			},
			l: listener,
		}
		s.servers = append(s.servers, httpServer)
	}
}

// Close closes servers and thus stop receiving requests
func (s *Server) Close() {
	for _, srv := range s.servers {
		if err := srv.Close(); err != nil {
			logrus.Error(err)
		}
	}
}

// serveAPI loops through all initialized servers and spawns goroutine
// with Serve method for each. It sets createMux() as Handler also.
func (s *Server) serveAPI() error {
	var chErrors = make(chan error, len(s.servers))
	for _, srv := range s.servers {
		srv.srv.Handler = s.routerSwapper
		go func(srv *HTTPServer) {
			var err error
			logrus.Infof("API listen on %s", srv.l.Addr())
			if err = srv.Serve(); err != nil && strings.Contains(err.Error(), "use of closed network connection") {
				err = nil
			}
			chErrors <- err
		}(srv)
	}

	for i := 0; i < len(s.servers); i++ {
		err := <-chErrors
		if err != nil {
			return err
		}
	}

	return nil
}

// HTTPServer contains an instance of http server and the listener.
// srv *http.Server, contains configuration to create an http server and a mux router with all api end points.
// l   net.Listener, is a TCP or Socket listener that dispatches incoming request to the router.
type HTTPServer struct {
	srv *http.Server
	l   net.Listener
}

// Serve starts listening for inbound requests.
func (s *HTTPServer) Serve() error {
	return s.srv.Serve(s.l)
}

// Close closes the HTTPServer from listening for the inbound requests.
func (s *HTTPServer) Close() error {
	return s.l.Close()
}

func (s *Server) makeHTTPHandler(handler httputils.APIFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Define the context that we'll pass around to share info
		// like the docker-request-id.
		//
		// The 'context' will be used for global data that should
		// apply to all requests. Data that is specific to the
		// immediate function being called should still be passed
		// as 'args' on the function call.
		ctx := context.WithValue(context.Background(), httputils.UAStringKey, r.Header.Get("User-Agent"))
		handlerFunc := s.handlerWithGlobalMiddlewares(handler)

		vars := mux.Vars(r)
		if vars == nil {
			vars = make(map[string]string)
		}

		if err := handlerFunc(ctx, w, r, vars); err != nil {
			statusCode := httputils.GetHTTPErrorStatusCode(err)
			errFormat := "%v"
			if statusCode == http.StatusInternalServerError {
				errFormat = "%+v"
			}
			logrus.Errorf("Handler for %s %s returned error: "+errFormat, r.Method, r.URL.Path, err)
			httputils.MakeErrorHandler(err)(w, r)
		}
	}
}

// InitRouter initializes the list of routers for the server.
// This method also enables the Go profiler if enableProfiler is true.
func (s *Server) InitRouter(enableProfiler bool, routers ...router.Router) {
	s.routers = append(s.routers, routers...) //将创建好的路由表信息追加到apiServer对象中的routers

	m := s.createMux() //追加后再次初始化apiServer路由器进行更新
	if enableProfiler {
		profilerSetup(m)
	}
	s.routerSwapper = &routerSwapper{ //这里设置好了mux.Route之后，将该route设置到apiServer的路由交换器中去，至此所有deamon.start（）的相关工作处理完毕
		router: m,
	}
}

// createMux initializes the main router the server uses.
func (s *Server) createMux() *mux.Router {
	/*
		mux位于vendor/github.com/gorilla/mux,该函数新建一个mux.go中的Route（路由数据项）对象并追加到mux.Router结构体中的成员routes中去，然后返回该路由器mux.Route m
	*/
	m := mux.NewRouter()

	logrus.Debug("Registering routers")
	//遍历所有apiserver中的api路由器如：container
	for _, apiRouter := range s.routers {
		//遍历每个apiRouter的子命令路由r如"/containers/create"
		for _, r := range apiRouter.Routes() {
			//给每个r的路由handler包裹了一层中间件（这里还不是很清楚）
			f := s.makeHTTPHandler(r.Handler())

			logrus.Debugf("Registering %s, %s", r.Method(), r.Path())
			/*
				在mux.Route路由结构中根据这个r.Path()路径设置一个适配器来匹配方法method和handler，当满足versionMatcher+r.Path()路径的正则表达式要求就可以适配到相应的方法名及该handler
			*/
			m.Path(versionMatcher + r.Path()).Methods(r.Method()).Handler(f)
			m.Path(r.Path()).Methods(r.Method()).Handler(f)
		}
	}

	err := errors.NewRequestNotFoundError(fmt.Errorf("page not found"))
	notFoundHandler := httputils.MakeErrorHandler(err)
	m.HandleFunc(versionMatcher+"/{path:.*}", notFoundHandler)
	m.NotFoundHandler = notFoundHandler

	return m
}

// Wait blocks the server goroutine until it exits.
// It sends an error message if there is any error during
// the API execution.
func (s *Server) Wait(waitChan chan error) {
	if err := s.serveAPI(); err != nil {
		logrus.Errorf("ServeAPI error: %v", err)
		waitChan <- err
		return
	}
	waitChan <- nil
}

// DisableProfiler reloads the server mux without adding the profiler routes.
func (s *Server) DisableProfiler() {
	s.routerSwapper.Swap(s.createMux())
}

// EnableProfiler reloads the server mux adding the profiler routes.
func (s *Server) EnableProfiler() {
	m := s.createMux()
	profilerSetup(m)
	s.routerSwapper.Swap(m)
}

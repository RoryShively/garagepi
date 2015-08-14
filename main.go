package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/gorilla/mux"
	"github.com/pivotal-golang/lager"
	"github.com/robdimsdale/garagepi/door"
	"github.com/robdimsdale/garagepi/fshelper"
	"github.com/robdimsdale/garagepi/gpio"
	"github.com/robdimsdale/garagepi/homepage"
	"github.com/robdimsdale/garagepi/httphelper"
	"github.com/robdimsdale/garagepi/light"
	"github.com/robdimsdale/garagepi/middleware"
	"github.com/robdimsdale/garagepi/oshelper"
	"github.com/robdimsdale/garagepi/webcam"
	"github.com/tedsuo/ifrit"
)

const (
	DEBUG = "debug"
	INFO  = "info"
	ERROR = "error"
	FATAL = "fatal"
)

var (
	// version is deliberately left uninitialized so it can be set at compile-time
	version string

	port = flag.Uint("port", 9999, "Port for server to bind to.")

	webcamHost = flag.String("webcamHost", "localhost", "Host of webcam image.")
	webcamPort = flag.Uint("webcamPort", 8080, "Port of webcam image.")

	gpioDoorPin  = flag.Uint("gpioDoorPin", 17, "Gpio pin of door.")
	gpioLightPin = flag.Uint("gpioLightPin", 2, "Gpio pin of light.")

	logLevel = flag.String("logLevel", string(INFO), "log level: debug, info, error or fatal")

	enableHTTPS = flag.Bool("enableHTTPS", false, "Enable HTTPS traffic")

	certFile = flag.String("certFile", "", "A PEM encoded certificate file.")
	keyFile  = flag.String("keyFile", "", "A PEM encoded private key file.")
	caFile   = flag.String("caFile", "", "A PEM encoded CA's certificate file.")

	username = flag.String("username", "", "Username for HTTP authentication.")
	password = flag.String("password", "", "Password for HTTP authentication.")
)

func main() {
	if version == "" {
		version = "dev"
	}

	flag.Parse()

	logger := initializeLogger()
	logger.Info("garagepi starting", lager.Data{"version": version})

	if *enableHTTPS {
		if *keyFile == "" {
			logger.Fatal("exiting", fmt.Errorf("keyFile must be provided if useHTTPS is true"))
		}

		if *certFile == "" {
			logger.Fatal("exiting", fmt.Errorf("certFile must be provided if useHTTPS is true"))
		}
	}

	// The location of the 'assets' directory
	// is relative to where the compilation takes place
	// This assumes compliation happens from the root directory
	// It is also apparently relative to the fshelper package.
	fsHelper := fshelper.NewFsHelperImpl("../assets")
	osHelper := oshelper.NewOsHelperImpl(logger)
	httpHelper := httphelper.NewHttpHelperImpl()

	rtr := mux.NewRouter()

	wh := webcam.NewHandler(
		logger,
		httpHelper,
		*webcamHost,
		*webcamPort,
	)

	gpio := gpio.NewGpio(osHelper, logger)

	lh := light.NewHandler(
		logger,
		httpHelper,
		gpio,
		*gpioLightPin,
	)

	hh := homepage.NewHandler(
		logger,
		httpHelper,
		fsHelper,
		lh,
	)

	dh := door.NewHandler(
		logger,
		httpHelper,
		osHelper,
		gpio,
		*gpioDoorPin)

	staticFileSystem, err := fsHelper.GetStaticFileSystem()
	if err != nil {
		panic(err)
	}

	staticFileServer := http.FileServer(staticFileSystem)
	strippedStaticFileServer := http.StripPrefix("/static/", staticFileServer)

	rtr.PathPrefix("/static/").Handler(strippedStaticFileServer)
	rtr.HandleFunc("/", hh.Handle).Methods("GET")
	rtr.HandleFunc("/webcam", wh.Handle).Methods("GET")
	rtr.HandleFunc("/toggle", dh.HandleToggle).Methods("POST")
	rtr.HandleFunc("/light", lh.HandleGet).Methods("GET")
	rtr.HandleFunc("/light", lh.HandleSet).Methods("POST")

	var tlsConfig *tls.Config
	if *enableHTTPS {
		// Load client cert
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			log.Fatal(err)
		}

		// Setup HTTPS client
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
		}

		if *caFile != "" {
			// Load CA cert
			caCert, err := ioutil.ReadFile(*caFile)
			if err != nil {
				log.Fatal(err)
			}
			caCertPool := x509.NewCertPool()
			caCertPool.AppendCertsFromPEM(caCert)

			tlsConfig.RootCAs = caCertPool
		}

		tlsConfig.BuildNameToCertificate()
	}

	runner := runner{
		port:      *port,
		logger:    logger,
		handler:   newHandler(rtr, logger),
		tlsConfig: tlsConfig,
	}

	process := ifrit.Invoke(runner)

	logger.Info("garagepi started")

	err = <-process.Wait()
	if err != nil {
		logger.Error("Error running garagepi", err)
	}
}

func initializeLogger() lager.Logger {
	var minLagerLogLevel lager.LogLevel
	switch *logLevel {
	case DEBUG:
		minLagerLogLevel = lager.DEBUG
	case INFO:
		minLagerLogLevel = lager.INFO
	case ERROR:
		minLagerLogLevel = lager.ERROR
	case FATAL:
		minLagerLogLevel = lager.FATAL
	default:
		panic(fmt.Errorf("unknown log level: %s", *logLevel))
	}

	logger := lager.NewLogger("garagepi")

	sink := lager.NewReconfigurableSink(lager.NewWriterSink(os.Stdout, lager.DEBUG), minLagerLogLevel)
	logger.RegisterSink(sink)

	return logger
}

type runner struct {
	port      uint
	logger    lager.Logger
	handler   http.Handler
	tlsConfig *tls.Config
}

func (r runner) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	var listener net.Listener
	var err error

	if *enableHTTPS {
		listener, err = tls.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", r.port), r.tlsConfig)
	} else {
		listener, err = net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", r.port))
	}
	if err != nil {
		return err
	} else {
		r.logger.Info("web server listening on port", lager.Data{"port": r.port})
	}

	errChan := make(chan error)
	go func() {
		err := http.Serve(listener, r.handler)
		if err != nil {
			errChan <- err
		}
	}()

	close(ready)

	select {
	case <-signals:
		return listener.Close()
	case err := <-errChan:
		return err
	}
}

func newHandler(mux http.Handler, logger lager.Logger) http.Handler {
	if *username == "" && *password == "" {
		return middleware.Chain{
			middleware.NewPanicRecovery(logger),
			middleware.NewLogger(logger),
		}.Wrap(mux)
	} else {
		return middleware.Chain{
			middleware.NewPanicRecovery(logger),
			middleware.NewLogger(logger),
			middleware.NewBasicAuth("username", "password"),
		}.Wrap(mux)
	}
}
